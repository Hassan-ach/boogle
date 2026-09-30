use lapin::options::BasicAckOptions;
use lapin::options::BasicNackOptions;
use tokio::sync::Mutex;
use tokio::sync::Semaphore;
use tokio::task::JoinSet;

use crate::core::config::AppConfig;
use crate::core::errors::AppError;
use crate::core::errors::DatabaseError;
use crate::core::messaging::IndexConfirmation;
use crate::core::messaging::Job;
use crate::core::messaging::MessagingQueue;
use crate::core::psql::DB;
use crate::core::text_sink::parse;
use crate::core::utils::retry_async;
use crate::core::utils::retry_sync;
use slog::{Logger, error, info};
use sqlx::prelude::FromRow;
use std::sync::Arc;
use tokio_stream::StreamExt;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

#[derive(Debug, FromRow)]
pub struct Page {
    pub id: Uuid,
    pub url_id: Uuid,
    pub html: String,
}

#[derive(Debug)]
pub struct Indexer<DBImpl: DB, MQImpl: MessagingQueue> {
    db: DBImpl,
    conf: AppConfig,
    mq: MQImpl,
    log: Logger,
    pub tasks: Mutex<JoinSet<()>>,
    limit: Arc<Semaphore>,
}

#[async_trait::async_trait]
pub trait Indexe {
    async fn start(self: Arc<Self>, tk: CancellationToken) -> Result<(), AppError>;
    async fn close(&self);
    async fn sweep_loop(self: Arc<Self>, tk: CancellationToken);
}

impl<DBImpl, MQImpl> Indexer<DBImpl, MQImpl>
where
    DBImpl: DB + Send + Sync + 'static,
    MQImpl: MessagingQueue + Send + Sync + 'static,
{
    pub fn new(db: DBImpl, mq: MQImpl, conf: AppConfig, log: Logger) -> Self {
        let max_concurrent_tasks = conf.max_concurrent_tasks;
        Indexer {
            db,
            mq,
            conf,
            log,
            tasks: Mutex::new(JoinSet::new()),
            limit: Arc::new(Semaphore::new(max_concurrent_tasks)),
        }
    }
    pub async fn handler(&self, body: Vec<u8>) -> Result<(), AppError> {
        let job: Job = serde_json::from_slice(&body)?;
        let page = self.db.get_page_by_id(job.page_id).await?;
        self.index_page(page).await?;
        Ok(())
    }

    pub async fn index_loop(self: Arc<Self>, tk: CancellationToken) -> Result<(), AppError> {
        if tk.is_cancelled() {
            info!(
                self.log,
                "indexing task received shutdown signal, stopping..."
            );
            return Ok(());
        }
        match retry_async(3, || async {
            self.mq
                .consume(&self.conf.queue_name, self.conf.max_concurrent_tasks)
                .await
        })
        .await
        {
            Ok(mut consumer) => loop {
                tokio::select! {
                    _ = tk.cancelled() => {
                        info!(self.log, "Received shutdown signal, stopping consumer for queue"; "queue" => self.conf.queue_name.to_string());
                        return Ok(());
                    }
                    result = consumer.next() => {
                        let Some(result) = result else {
                            info!(self.log, "Consumer stream ended for queue"; "queue" => self.conf.queue_name.to_string());
                            return Ok(());
                        };

                        let delivery = result?;
                        let indx = Arc::clone(&self);
                        let log = indx.log.clone();
                        let queue_name = indx.conf.queue_name.clone();

                        info!(log, "Received message from queue"; "queue" => &queue_name, "payload_size" => delivery.data.len());

                        // Acquire semaphore slot, respecting cancellation
                        let permit = tokio::select! {
                            Ok(p) = Arc::clone(&indx.limit).acquire_owned() => p,
                            _ = tk.cancelled() => {
                                info!(log, "Received shutdown signal while waiting for permit, stopping"; "queue" => queue_name.clone());
                                return Ok(());
                            }
                        };

                        let mut tasks = self.tasks.lock().await;
                        tasks.spawn(async move {
                            let _permit = permit; // Keeps slot active until task completes

                            match indx.handler(delivery.data.clone()).await {
                                Ok(()) => {
                                    if let Err(err) = delivery.ack(BasicAckOptions::default()).await {
                                        error!(log.clone(), "ACK error"; "err" => %err, "queue" => queue_name.clone());
                                    }
                                }
                                Err(err) => {
                                    // Determine logging and re-queue policy declaratively
                                    let requeue = match &err {
                                        AppError::Database(db_err) => {
                                            error!(log.clone(), "Database error"; "err" => %db_err, "queue" => queue_name.clone());
                                            !matches!(db_err, DatabaseError::NotFoundError(_)) // Requeue unless not found
                                        }
                                        AppError::Messaging(msg_err) => {
                                            error!(log.clone(), "Messaging error"; "err" => %msg_err, "queue" => queue_name.clone());
                                            true
                                        }
                                        AppError::SerdeError(serde_err) => {
                                            error!(log.clone(), "Serde error"; "err" => %serde_err, "queue" => queue_name.clone());
                                            false // Poison message, do not requeue
                                        }
                                        AppError::Other(msg) => {
                                            error!(log.clone(), "Other error"; "err" => msg, "queue" => queue_name.clone());
                                            true
                                        }
                                        AppError::ParseError(msg) => {
                                            error!(log.clone(), "Parse error"; "err" => msg, "queue" => queue_name.clone());
                                            false // Poison message, do not requeue
                                        }
                                    };
                                    if let Err(nack_err) = delivery.nack(BasicNackOptions { requeue, ..Default::default() }).await {
                                        error!(log.clone(), "NACK error"; "err" => %nack_err, "queue" => queue_name.clone());
                                    }
                                }
                            }
                        });
                    }
                }

                let mut tasks = self.tasks.lock().await;

                while let Some(result) = tasks.try_join_next() {
                    if let Err(err) = result {
                        error!(self.log.clone(), "Indexer worker task failed"; "error" => %err);
                    }
                }
            },

            Err(err) => {
                error!(self.log, "failed to consume messages from queue"; "error" => err.to_string());
                return Err(err);
            }
        }
    }

    pub async fn index_page(&self, page: Page) -> Result<(), AppError> {
        info!(self.log, "handling page for indexing";
             "page_id" => page.id.to_string(),
             "url_id" => page.url_id.to_string(),
             "html_size" => page.html.len()
        );
        let html = page.html;
        let words = match tokio::task::spawn_blocking(move || parse(html)).await {
            Ok(Ok(words)) => words,
            Ok(Err(err)) => {
                error!(self.log, "failed to parse page";
                       "page_id" => page.id.to_string(),
                       "url_id" => page.url_id.to_string(),
                       "error" => err.to_string()
                );
                self.db.undo_indexing(page.id).await?;
                return Err(AppError::ParseError(format!(
                    "Failed to parse page: {}",
                    err
                )));
            }
            Err(err) => {
                error!(self.log, "failed to spawn blocking task for parsing";
                       "page_id" => page.id.to_string(),
                       "url_id" => page.url_id.to_string(),
                       "error" => err.to_string()
                );
                self.db.undo_indexing(page.id).await?;
                return Err(AppError::Other(format!(
                    "Failed to spawn blocking task: {}",
                    err
                )));
            }
        };

        match retry_async(3, || async { self.db.batch_words(&words, page.id).await }).await {
            Ok(_) => {
                info!(self.log, "successfully indexed page";
                     "page_id" => page.id.to_string(),
                     "url_id" => page.url_id.to_string(),
                     "word_count" => words.len()
                );
            }
            Err(err) => {
                error!(self.log, "failed to batch words for page";
                       "page_id" => page.id.to_string(),
                       "url_id" => page.url_id.to_string(),
                       "error" => err.to_string()
                );
                self.db.undo_indexing(page.id).await?;
                return Ok(());
            }
        }

        match retry_sync(3, || {
            serde_json::to_vec(&IndexConfirmation { page_id: page.id })
        }) {
            Ok(payload) => {
                // It ok to ignore the result of the publish_with_confirm call, since we don't want to block the indexing process.
                if let Err(err) = retry_async(3, || async {
                    self.mq
                        .publish_with_confirm(&self.conf.confirmation_queue_name, payload.clone())
                        .await
                })
                .await
                {
                    error!(self.log, "failed to publish index confirmation";
                           "page_id" => page.id.to_string(),
                           "url_id" => page.url_id.to_string(),
                           "error" => err.to_string()
                    );
                }
            }
            Err(err) => {
                error!(self.log, "failed to serialize index confirmation";
                       "page_id" => page.id.to_string(),
                       "url_id" => page.url_id.to_string(),
                       "error" => err.to_string()
                );
            }
        }

        return Ok(());
    }
}

#[async_trait::async_trait]
impl<DBImpl, MQImpl> Indexe for Indexer<DBImpl, MQImpl>
where
    DBImpl: DB + Sync + Send + 'static,
    MQImpl: MessagingQueue + Send + Sync + 'static,
{
    async fn start(self: Arc<Self>, tk: CancellationToken) -> Result<(), AppError> {
        let tk_clone = tk.clone();
        let res = Arc::clone(&self).index_loop(tk_clone).await;
        tk.cancel();
        res
    }

    async fn close(&self) {
        self.mq.close().await;
        self.db.close().await;
    }

    async fn sweep_loop(self: Arc<Self>, tk: CancellationToken) {
        let mut ticker = tokio::time::interval_at(
            tokio::time::Instant::now() + self.conf.sweep_interval,
            self.conf.sweep_interval,
        );
        loop {
            tokio::select! {
                _ = tk.cancelled() => {
                    info!(self.log, "Sweep loop stopping");
                    return;
                }
                _ = ticker.tick() => {}
            }

            match self
                .db
                .get_stale_unindexed(self.conf.sweep_grace, 500)
                .await
            {
                Ok(ids) if ids.is_empty() => {}
                Ok(ids) => {
                    info!(self.log, "Sweep found stale unindexed pages"; "count" => ids.len());
                    for id in ids {
                        let job = Job { page_id: id };
                        match serde_json::to_vec(&job) {
                            Ok(body) => {
                                if let Err(e) = retry_async(3, || async {
                                    self.mq.publish(&self.conf.queue_name, body.clone()).await
                                })
                                .await
                                {
                                    error!(self.log, "Sweep publish failed"; "page_id" => %id, "err" => %e);
                                }
                            }
                            Err(e) => {
                                error!(self.log, "Sweep serialize failed"; "page_id" => %id, "err" => %e)
                            }
                        }
                    }
                }
                Err(e) => error!(self.log, "Sweep query failed"; "err" => %e),
            }
        }
    }
}
