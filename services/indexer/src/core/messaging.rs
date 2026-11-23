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

#[derive(Clone)]
pub struct RabbitMQ {
    conf: RabbitConfig,
    state: Arc<tokio::sync::RwLock<BrokerState>>,
    log: Arc<Logger>,
}

struct BrokerState {
    conn: Connection,
    ch: Channel,
    confirm_ch: Channel,
}

#[cfg(test)]
mod tests {
    use super::*;

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
        let state = Self::connect(&conf, &log).await?;
        Ok(RabbitMQ {
            conf,
            state: Arc::new(tokio::sync::RwLock::new(state)),
            log: Arc::new(log),
        })
    }

    /// Dial the broker and prepare both channels, including the queue
    /// declarations. Used at startup and again after the connection dies.
    async fn connect(conf: &RabbitConfig, log: &Logger) -> Result<BrokerState, AppError> {
        let props = ConnectionProperties::default().enable_auto_recover();
        let conn = Connection::connect(&conf.url, props).await.map_err(|err| {
            error!(log, "Failed to connect to RabbitMQ"; "err" => %err);
            AppError::Messaging(MessagingError::ConnectionError(format!(
                "Failed to connect to RabbitMQ: {}",
                err.to_string()
            )))
        })?;

        let ch = conn.create_channel().await.map_err(|err| {
            error!(log, "Failed to create channel"; "err" => %err);
            AppError::Messaging(MessagingError::ChannelError(format!(
                "Failed to create channel: {}",
                err.to_string()
            )))
        })?;

        let confirm_ch = conn.create_channel().await.map_err(|err| {
            error!(log, "Failed to create channel"; "err" => %err);
            AppError::Messaging(MessagingError::ChannelError(format!(
                "Failed to create channel: {}",
                err.to_string()
            )))
        })?;

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

        for queue in [&conf.queue, &conf.confirmation_queue_name] {
            ch.queue_declare(
                ShortString::from(queue.as_str()),
                QueueDeclareOptions::durable(),
                FieldTable::default(),
            )
            .await
            .map_err(|err| {
                error!(log, "Failed to declare queue"; "queue" => queue, "err" => %err);
                AppError::Messaging(MessagingError::DeclareQueueError(format!(
                    "Failed to declare queue {}: {}",
                    queue,
                    err.to_string()
                )))
            })?;
            info!(log, "Queue declared successfully"; "queue" => queue);
        }

        info!(log, "Connected to RabbitMQ successfully"; "queue" => &conf.queue);
        Ok(BrokerState {
            conn,
            ch,
            confirm_ch,
        })
    }

    /// Return live channels, reconnecting first if the connection has died.
    ///
    /// lapin does not re-dial by itself, so a dead connection must be replaced
    /// before it is reused.
    async fn channels(&self) -> Result<(Channel, Channel), AppError> {
        {
            let state = self.state.read().await;
            if state.conn.status().connected() {
                return Ok((state.ch.clone(), state.confirm_ch.clone()));
            }
        }

        let mut state = self.state.write().await;
        if state.conn.status().connected() {
            return Ok((state.ch.clone(), state.confirm_ch.clone()));
        }

        info!(self.log.as_ref(), "RabbitMQ connection lost; reconnecting");
        *state = Self::connect(&self.conf, self.log.as_ref()).await?;
        Ok((state.ch.clone(), state.confirm_ch.clone()))
    }

    /// Force a fresh connection after a channel-level failure even if the
    /// connection status still reports connected.
    async fn force_reconnect(&self) {
        let mut state = self.state.write().await;
        if let Ok(fresh) = Self::connect(&self.conf, self.log.as_ref()).await {
            *state = fresh;
        }
    }
}

#[async_trait::async_trait]
impl MessagingQueue for RabbitMQ {
    async fn publish(&self, queue: &str, payload: Vec<u8>) -> Result<(), AppError> {
        let (ch, _) = self.channels().await?;
        ch.basic_publish(
            ShortString::from(""),
            ShortString::from(queue),
            BasicPublishOptions::default(),
            &payload,
            BasicProperties::default()
                .with_delivery_mode(2)
                .with_content_type("application/json".into()),
        )
        .await
        .map_err(|err| {
            error!(self.log.as_ref(), "Failed to publish message"; "err" => %err);
            AppError::Messaging(MessagingError::PublishError(format!(
                "Failed to publish message: {}",
                err.to_string()
            )))
        })?;

        Ok(())
    }

    async fn publish_with_confirm(&self, queue: &str, payload: Vec<u8>) -> Result<(), AppError> {
        let (_, ch) = self.channels().await?;
        let conf = ch
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
            .map_err(|err| {
                error!(self.log.as_ref(), "Failed to publish message"; "err" => %err);
                AppError::Messaging(MessagingError::PublishError(format!(
                    "Failed to publish message: {}",
                    err.to_string()
                )))
            })?;

        match conf.await? {
            Confirmation::Ack(None) => {}
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
            Confirmation::NotRequested => {}
        }
        Ok(())
    }

    async fn consume(&self, queue: &str, prefetch: usize) -> Result<Consumer, AppError> {
        let (ch, _) = self.channels().await?;

        if let Err(err) = ch
            .basic_qos(prefetch as u16, BasicQosOptions::default())
            .await
        {
            self.force_reconnect().await;
            return Err(AppError::Messaging(MessagingError::ConsumeError(format!(
                "failed to set prefetch {prefetch} on queue {queue}: {err}"
            ))));
        }

        match ch
            .basic_consume(
                ShortString::from(queue),
                ShortString::from(""),
                BasicConsumeOptions::default(),
                FieldTable::default(),
            )
            .await
        {
            Ok(consumer) => Ok(consumer),
            Err(err) => {
                error!(self.log.as_ref(), "Failed to consume messages"; "err" => %err);
                self.force_reconnect().await;
                Err(AppError::Messaging(MessagingError::ConsumeError(format!(
                    "Failed to consume messages: {}",
                    err.to_string()
                ))))
            }
        }
    }

    async fn close(&self) {
        let state = self.state.read().await;
        if let Err(err) = state.conn.close(0, "Closing connection".into()).await {
            error!(self.log.as_ref(), "Failed to close RabbitMQ connection"; "err" => %err);
        }
        info!(self.log.as_ref(), "RabbitMQ connection closed successfully");
    }
}
