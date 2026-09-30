use std::sync::Arc;

use lapin::{
    BasicProperties, Channel, Confirmation, Connection, ConnectionProperties, Consumer,
    options::{BasicConsumeOptions, BasicPublishOptions, BasicQosOptions, QueueDeclareOptions},
    types::{FieldTable, ShortString},
};
use serde::{Deserialize, Serialize};
use slog::{Logger, error, info};

use crate::core::errors::AppError;
use crate::core::{config::RabbitConfig, errors::MessagingError};

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Job {
    pub page_id: uuid::Uuid,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct IndexConfirmation {
    pub page_id: uuid::Uuid,
}

#[derive(Debug, Clone)]
pub struct RabbitMQ {
    conn: Arc<Connection>,
    ch: Channel,
    confirm_ch: Channel,
    log: Arc<Logger>,
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The spider service (Go) publishes `{"page_id":"<uuid>"}`. If this shape
    /// drifts, the indexer rejects every job with a serde error and the crawl
    /// silently stops indexing, so the wire format is pinned here.
    const SPIDER_PAYLOAD: &str = r#"{"page_id":"6f8c3c3c-1b1a-4b2e-9f3d-2c4e5f6a7b8c"}"#;

    #[test]
    fn job_deserializes_the_spider_payload() {
        let job: Job = serde_json::from_str(SPIDER_PAYLOAD).expect("spider payload must parse");
        assert_eq!(
            job.page_id.to_string(),
            "6f8c3c3c-1b1a-4b2e-9f3d-2c4e5f6a7b8c"
        );
    }

    #[test]
    fn job_round_trips_through_serde() {
        let id = uuid::Uuid::new_v4();
        let job = Job { page_id: id };
        let encoded = serde_json::to_vec(&job).unwrap();
        let decoded: Job = serde_json::from_slice(&encoded).unwrap();
        assert_eq!(decoded.page_id, id);
    }

    #[test]
    fn job_encodes_page_id_as_a_bare_string() {
        let id = uuid::Uuid::new_v4();
        let encoded = serde_json::to_string(&Job { page_id: id }).unwrap();
        assert_eq!(encoded, format!(r#"{{"page_id":"{id}"}}"#));
    }

    #[test]
    fn job_uses_the_same_wire_shape_as_index_confirmation() {
        // Both messages cross a service boundary, so they must stay compatible
        // with the Go producer.
        let id = uuid::Uuid::new_v4();
        assert_eq!(
            serde_json::to_string(&Job { page_id: id }).unwrap(),
            serde_json::to_string(&IndexConfirmation { page_id: id }).unwrap()
        );
    }

    #[test]
    fn malformed_job_payloads_are_rejected() {
        for bad in [
            "",
            "{",
            "{}",
            r#"{"page_id":""}"#,
            r#"{"page_id":"not-a-uuid"}"#,
            r#"{"pageid":"6f8c3c3c-1b1a-4b2e-9f3d-2c4e5f6a7b8c"}"#,
        ] {
            let parsed = serde_json::from_str::<Job>(bad);
            assert!(parsed.is_err(), "expected {bad:?} to be rejected");
        }
    }

    #[test]
    fn index_confirmation_round_trips() {
        let id = uuid::Uuid::new_v4();
        let encoded = serde_json::to_vec(&IndexConfirmation { page_id: id }).unwrap();
        let decoded: IndexConfirmation = serde_json::from_slice(&encoded).unwrap();
        assert_eq!(decoded.page_id, id);
    }

    #[test]
    fn extra_fields_are_tolerated() {
        // Forward compatibility: a newer producer must not break this consumer.
        let payload = r#"{"page_id":"6f8c3c3c-1b1a-4b2e-9f3d-2c4e5f6a7b8c","attempt":2}"#;
        let job: Job = serde_json::from_str(payload).unwrap();
        assert!(!job.page_id.is_nil());
    }
}

#[async_trait::async_trait]
pub trait MessagingQueue: Send + Sync {
    async fn publish(&self, queue: &str, payload: Vec<u8>) -> Result<(), AppError>;
    async fn publish_with_confirm(&self, queue: &str, payload: Vec<u8>) -> Result<(), AppError>;

    async fn consume(&self, queue: &str, prefetch: usize) -> Result<Consumer, AppError>;

    async fn close(&self);
}

impl RabbitMQ {
    pub async fn new(conf: RabbitConfig, log: Logger) -> Result<RabbitMQ, AppError> {
        let props = ConnectionProperties::default().enable_auto_recover();
        let conn = match Connection::connect(&conf.url, props).await {
            Ok(conn) => conn,
            Err(err) => {
                error!(log, "Failed to connect to RabbitMQ"; "err" => %err);
                return Err(AppError::Messaging(MessagingError::ConnectionError(
                    format!("Failed to connect to RabbitMQ: {}", err.to_string()),
                )));
            }
        };
        let ch = match conn.create_channel().await {
            Ok(ch) => ch,
            Err(err) => {
                error!(log, "Failed to create channel"; "err" => %err);
                return Err(AppError::Messaging(MessagingError::ChannelError(format!(
                    "Failed to create channel: {}",
                    err.to_string()
                ))));
            }
        };
        let confirm_ch = match conn.create_channel().await {
            Ok(ch) => ch,
            Err(err) => {
                error!(log, "Failed to create channel"; "err" => %err);
                return Err(AppError::Messaging(MessagingError::ChannelError(format!(
                    "Failed to create channel: {}",
                    err.to_string()
                ))));
            }
        };
        confirm_ch
            .confirm_select(lapin::options::ConfirmSelectOptions::default())
            .await
            .map_err(|err| {
                error!(log, "Failed to enable confirm select"; "err" => %err);
                AppError::Messaging(MessagingError::ChannelError(format!(
                    "Failed to enable confirm select: {}",
                    err.to_string()
                )))
            })?;

        let mq = RabbitMQ {
            conn: Arc::new(conn),
            ch: ch,
            log: Arc::new(log.clone()),
            confirm_ch: confirm_ch,
        };

        mq.queue_declare(&conf.queue).await?;
        mq.queue_declare(&conf.confirmation_queue_name).await?;

        info!(log, "Connected to RabbitMQ successfully"; "queue" => &conf.queue);

        Ok(mq)
    }

    async fn get_channel(&self) -> Channel {
        let ch = match self.conn.create_channel().await {
            Ok(ch) => ch,
            Err(err) => {
                error!(self.log.as_ref(), "Failed to create channel"; "err" => %err);
                return self.ch.clone(); // Return the existing channel if creation fails
            }
        };

        return ch;
    }
    async fn queue_declare(&self, queue: &str) -> Result<(), AppError> {
        let ch = self.get_channel().await;
        let _ = match ch
            .queue_declare(
                ShortString::from(queue),
                QueueDeclareOptions::durable(),
                FieldTable::default(),
            )
            .await
        {
            Ok(_) => {
                info!(self.log.as_ref(), "Queue declared successfully"; "queue" => queue);
            }
            Err(err) => {
                error!(self.log.as_ref(), "Failed to declare queue"; "queue" => queue, "err" => %err);
                return Err(AppError::Messaging(MessagingError::DeclareQueueError(
                    format!("Failed to declare queue {}: {}", queue, err.to_string()),
                )));
            }
        };
        Ok(())
    }
}

#[async_trait::async_trait]
impl MessagingQueue for RabbitMQ {
    async fn publish(&self, queue: &str, payload: Vec<u8>) -> Result<(), AppError> {
        let ch = self.ch.clone();
        match ch
            .basic_publish(
                ShortString::from(""),
                ShortString::from(queue),
                BasicPublishOptions::default(),
                &payload,
                BasicProperties::default()
                    .with_delivery_mode(2)
                    .with_content_type("application/json".into()),
            )
            .await
        {
            Ok(_) => {}
            Err(err) => {
                error!(self.log.as_ref(), "Failed to publish message"; "err" => %err);
                return Err(AppError::Messaging(MessagingError::PublishError(format!(
                    "Failed to publish message: {}",
                    err.to_string()
                ))));
            }
        };

        Ok(())
    }

    async fn publish_with_confirm(&self, queue: &str, payload: Vec<u8>) -> Result<(), AppError> {
        let ch = self.confirm_ch.clone();
        let conf = match ch
            .basic_publish(
                ShortString::from(""),
                ShortString::from(queue),
                BasicPublishOptions {
                    mandatory: true,
                    ..Default::default()
                },
                &payload,
                BasicProperties::default()
                    .with_delivery_mode(2)
                    .with_content_type("application/json".into()),
            )
            .await
        {
            Ok(c) => c,
            Err(err) => {
                error!(self.log.as_ref(), "Failed to publish message"; "err" => %err);
                return Err(AppError::Messaging(MessagingError::PublishError(format!(
                    "Failed to publish message: {}",
                    err.to_string()
                ))));
            }
        };

        match conf.await? {
            Confirmation::Ack(None) => {} // routed, persisted
            Confirmation::Ack(Some(m)) | Confirmation::Nack(Some(m)) => {
                return Err(AppError::Messaging(MessagingError::PublishError(format!(
                    "Message was not routed to a queue: reply code: {}, reply text: {}",
                    m.reply_code, m.reply_text
                ))));
            }
            Confirmation::Nack(None) => {
                return Err(AppError::Messaging(MessagingError::PublishError(
                    "Message was not acknowledged by the broker".to_string(),
                )));
            }
            Confirmation::NotRequested => { /* confirms not enabled — shouldn't happen here */ }
        }
        Ok(())
    }
    async fn consume(&self, queue: &str, prefetch: usize) -> Result<Consumer, AppError> {
        let ch = self.get_channel().await;

        ch.basic_qos(prefetch as u16, BasicQosOptions::default())
            .await
            .map_err(|err| {
                AppError::Messaging(MessagingError::ConsumeError(format!(
                    "failed to set prefetch {prefetch} on queue {queue}: {err}"
                )))
            })?;

        let consumer = match ch
            .basic_consume(
                ShortString::from(queue),
                ShortString::from(""),
                BasicConsumeOptions::default(),
                FieldTable::default(),
            )
            .await
        {
            Ok(consumer) => consumer,
            Err(err) => {
                error!(self.log.as_ref(), "Failed to consume messages"; "err" => %err);
                return Err(AppError::Messaging(MessagingError::ConsumeError(format!(
                    "Failed to consume messages: {}",
                    err.to_string()
                ))));
            }
        };

        return Ok(consumer);
    }

    async fn close(&self) {
        if let Err(err) = self.conn.close(0, "Closing connection".into()).await {
            error!(self.log.as_ref(), "Failed to close RabbitMQ connection"; "err" => %err);
        }
        info!(self.log.as_ref(), "RabbitMQ connection closed successfully");
    }
}
