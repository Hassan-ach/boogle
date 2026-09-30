use lapin::Consumer;
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

/// Total attempts for retryable operations (see `core::utils::retry_async`).
pub const MAX_ATTEMPTS: usize = 3;

#[derive(Debug, Clone, FromRow)]
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
    async fn start(self: Arc<Self>, tk: CancellationToken) -> Result<Consumer, AppError>;
    async fn close(&self);
    async fn sweep_loop(self: Arc<Self>, tk: CancellationToken);
}

/// Decide whether a failed job should go back on the queue.
///
/// Retrying a permanently broken message would spin the worker forever, so
/// poison payloads (undecodable JSON, unparsable HTML, a page that is already
/// gone) are discarded. Everything else is transient and gets requeued.
pub fn should_requeue(err: &AppError) -> bool {
    match err {
        AppError::Database(db_err) => !matches!(db_err, DatabaseError::NotFoundError(_)),
        AppError::Messaging(_) => true,
        AppError::SerdeError(_) => false,
        AppError::Other(_) => true,
        AppError::ParseError(_) => false,
    }
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

    /// Returns the still-open [`Consumer`] so the caller can keep it alive while
    /// in-flight workers drain. Dropping a lapin `Consumer` closes its channel,
    /// which makes every outstanding ack fail with `InvalidChannel`.
    pub async fn index_loop(self: Arc<Self>, tk: CancellationToken) -> Result<Consumer, AppError> {
        if tk.is_cancelled() {
            info!(
                self.log,
                "indexing task received shutdown signal, stopping..."
            );
            return Err(AppError::Other(
                "indexing task received shutdown signal before consuming".to_string(),
            ));
        }
        match retry_async(MAX_ATTEMPTS, || async {
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
                        return Ok(consumer);
                    }
                    result = consumer.next() => {
                        let Some(result) = result else {
                            info!(self.log, "Consumer stream ended for queue"; "queue" => self.conf.queue_name.to_string());
                            return Ok(consumer);
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
                                return Ok(consumer);
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
                                    let requeue = should_requeue(&err);
                                    match &err {
                                        AppError::Database(db_err) => error!(log.clone(), "Database error"; "err" => %db_err, "queue" => queue_name.clone()),
                                        AppError::Messaging(msg_err) => error!(log.clone(), "Messaging error"; "err" => %msg_err, "queue" => queue_name.clone()),
                                        AppError::SerdeError(serde_err) => error!(log.clone(), "Serde error"; "err" => %serde_err, "queue" => queue_name.clone()),
                                        AppError::Other(msg) => error!(log.clone(), "Other error"; "err" => msg, "queue" => queue_name.clone()),
                                        AppError::ParseError(msg) => error!(log.clone(), "Parse error"; "err" => msg, "queue" => queue_name.clone()),
                                    }
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

        match retry_async(MAX_ATTEMPTS, || async {
            self.db.batch_words(&words, page.id).await
        })
        .await
        {
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
                // The claim is released, so the sweep can pick the page up again.
                // Returning Ok here would make the caller ack the message and the
                // job would be dropped from the queue until the next sweep.
                return Err(err);
            }
        }

        match retry_sync(MAX_ATTEMPTS, || {
            serde_json::to_vec(&IndexConfirmation { page_id: page.id })
        }) {
            Ok(payload) => {
                // It ok to ignore the result of the publish_with_confirm call, since we don't want to block the indexing process.
                if let Err(err) = retry_async(MAX_ATTEMPTS, || async {
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
    async fn start(self: Arc<Self>, tk: CancellationToken) -> Result<Consumer, AppError> {
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
                                if let Err(e) = retry_async(MAX_ATTEMPTS, || async {
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::core::errors::DatabaseError;
    use crate::core::messaging::IndexConfirmation;
    use slog::{Drain, o};
    use std::collections::HashMap;
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::Duration;

    fn discard_logger() -> Logger {
        Logger::root(slog::Discard.fuse(), o!())
    }

    fn app_config() -> AppConfig {
        AppConfig {
            log_path: "test.log".into(),
            queue_name: "indexer.jobs".into(),
            max_concurrent_tasks: 4,
            sweep_interval: Duration::from_secs(300),
            sweep_grace: Duration::from_secs(600),
            confirmation_queue_name: "indexer.confirmations".into(),
        }
    }

    #[derive(Default)]
    struct MockDB {
        page: Mutex<Option<Page>>,
        batch_failures_remaining: AtomicUsize,
        undo_calls: AtomicUsize,
        closed: AtomicUsize,
        batched_words: Mutex<Vec<HashMap<String, u32>>>,
        stale: Mutex<Vec<Uuid>>,
        sweep_calls: AtomicUsize,
    }

    impl MockDB {
        fn with_page(html: &str) -> Self {
            Self {
                page: Mutex::new(Some(Page {
                    id: Uuid::new_v4(),
                    url_id: Uuid::new_v4(),
                    html: html.to_string(),
                })),
                ..Default::default()
            }
        }

        fn failing_batch(failures: usize) -> Self {
            Self {
                page: Mutex::new(Some(Page {
                    id: Uuid::new_v4(),
                    url_id: Uuid::new_v4(),
                    html: "<body>alpha beta</body>".into(),
                })),
                batch_failures_remaining: AtomicUsize::new(failures),
                ..Default::default()
            }
        }

        fn page_id(&self) -> Uuid {
            self.page.lock().unwrap().as_ref().unwrap().id
        }
    }

    #[async_trait::async_trait]
    impl DB for MockDB {
        async fn get_page_by_id(&self, _page_id: Uuid) -> Result<Page, AppError> {
            Ok(self.page.lock().unwrap().clone().unwrap())
        }

        async fn batch_words(
            &self,
            words: &HashMap<String, u32>,
            _page_id: Uuid,
        ) -> Result<(), AppError> {
            let remaining = self.batch_failures_remaining.load(Ordering::SeqCst);
            if remaining > 0 {
                self.batch_failures_remaining
                    .store(remaining - 1, Ordering::SeqCst);
                return Err(AppError::Database(DatabaseError::BatchWordsError(
                    "transient failure".into(),
                )));
            }
            self.batched_words.lock().unwrap().push(words.clone());
            Ok(())
        }

        async fn undo_indexing(&self, _page_id: Uuid) -> Result<(), AppError> {
            self.undo_calls.fetch_add(1, Ordering::SeqCst);
            Ok(())
        }

        async fn close(&self) {
            self.closed.fetch_add(1, Ordering::SeqCst);
        }

        async fn get_stale_unindexed(
            &self,
            _older_than: Duration,
            _limit: i64,
        ) -> Result<Vec<Uuid>, AppError> {
            self.sweep_calls.fetch_add(1, Ordering::SeqCst);
            Ok(self.stale.lock().unwrap().clone())
        }
    }

    #[derive(Default)]
    struct MockMQ {
        published: Mutex<Vec<(String, Vec<u8>)>>,
        confirmed: Mutex<Vec<(String, Vec<u8>)>>,
        publish_failures_remaining: AtomicUsize,
        closed: AtomicUsize,
    }

    #[async_trait::async_trait]
    impl MessagingQueue for MockMQ {
        async fn publish(&self, queue: &str, payload: Vec<u8>) -> Result<(), AppError> {
            self.published
                .lock()
                .unwrap()
                .push((queue.to_string(), payload));
            Ok(())
        }

        async fn publish_with_confirm(
            &self,
            queue: &str,
            payload: Vec<u8>,
        ) -> Result<(), AppError> {
            let remaining = self.publish_failures_remaining.load(Ordering::SeqCst);
            if remaining > 0 {
                self.publish_failures_remaining
                    .store(remaining - 1, Ordering::SeqCst);
                return Err(AppError::Messaging(
                    crate::core::errors::MessagingError::PublishError("nack".into()),
                ));
            }
            self.confirmed
                .lock()
                .unwrap()
                .push((queue.to_string(), payload));
            Ok(())
        }

        async fn consume(&self, _queue: &str, _prefetch: usize) -> Result<Consumer, AppError> {
            // Consuming needs a live broker; these tests drive `index_page`
            // directly instead.
            Err(AppError::Other("not supported in tests".into()))
        }

        async fn close(&self) {
            self.closed.fetch_add(1, Ordering::SeqCst);
        }
    }

    // ── index_page ────────────────────────────────────────────────────────────

    #[tokio::test]
    async fn index_page_indexes_words_and_publishes_a_confirmation() {
        let db = MockDB::with_page("<body>alpha beta alpha</body>");
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());

        let page = indexer.db.page.lock().unwrap().clone().unwrap();
        let result = indexer.index_page(page).await;

        assert!(result.is_ok(), "unexpected error: {result:?}");

        let batched = indexer.db.batched_words.lock().unwrap();
        assert_eq!(batched.len(), 1);
        assert_eq!(batched[0].get("alpha"), Some(&2));
        assert_eq!(batched[0].get("beta"), Some(&1));

        let confirmed = indexer.mq.confirmed.lock().unwrap();
        assert_eq!(confirmed.len(), 1, "exactly one confirmation");
        assert_eq!(confirmed[0].0, "indexer.confirmations");

        let parsed: IndexConfirmation = serde_json::from_slice(&confirmed[0].1).unwrap();
        assert_eq!(parsed.page_id, indexer.db.page_id());

        assert_eq!(
            indexer.db.undo_calls.load(Ordering::SeqCst),
            0,
            "a successful index must not undo the claim"
        );
    }

    #[tokio::test]
    async fn index_page_handles_a_page_with_no_indexable_words() {
        let db = MockDB::with_page("<body><script>var x=1</script></body>");
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());
        let page = indexer.db.page.lock().unwrap().clone().unwrap();

        let result = indexer.index_page(page).await;

        assert!(result.is_ok());
        let batched = indexer.db.batched_words.lock().unwrap();
        assert_eq!(batched.len(), 1);
        assert!(batched[0].is_empty(), "script text must not be indexed");
    }

    #[tokio::test]
    async fn index_page_succeeds_on_empty_html() {
        let db = MockDB::with_page("");
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());
        let page = indexer.db.page.lock().unwrap().clone().unwrap();

        assert!(indexer.index_page(page).await.is_ok());
    }

    #[tokio::test]
    async fn index_page_does_not_panic_on_pathological_html() {
        // Very large and deeply malformed input must still produce a result.
        let huge = format!(
            "<body>{}<div><span>{}{}",
            "<p>filler text </p>".repeat(5_000),
            "<b>".repeat(500),
            "</b>".repeat(500)
        );
        let db = MockDB::with_page(&huge);
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());
        let page = indexer.db.page.lock().unwrap().clone().unwrap();

        assert!(indexer.index_page(page).await.is_ok());
    }

    #[tokio::test(start_paused = true)]
    async fn index_page_retries_transient_batch_failures_then_succeeds() {
        let db = MockDB::failing_batch(2);
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());
        let page = indexer.db.page.lock().unwrap().clone().unwrap();

        let result = indexer.index_page(page).await;

        assert!(result.is_ok(), "two failures fit inside the retry budget");
        assert_eq!(indexer.db.undo_calls.load(Ordering::SeqCst), 0);
        assert_eq!(indexer.db.batched_words.lock().unwrap().len(), 1);
    }

    #[tokio::test(start_paused = true)]
    async fn index_page_releases_the_claim_when_batching_exhausts_retries() {
        // Regression: this used to return Ok(()), which made the caller ack the
        // message and silently dropped the job.
        let db = MockDB::failing_batch(99);
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());
        let page = indexer.db.page.lock().unwrap().clone().unwrap();

        let result = indexer.index_page(page).await;

        let err = result.expect_err("exhausted retries must surface an error");
        assert!(
            matches!(err, AppError::Database(DatabaseError::BatchWordsError(_))),
            "unexpected error kind: {err:?}"
        );
        assert_eq!(
            indexer.db.undo_calls.load(Ordering::SeqCst),
            1,
            "the page claim must be released so the sweep can retry it"
        );
        assert!(
            indexer.mq.confirmed.lock().unwrap().is_empty(),
            "an unindexed page must not trigger ranking"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn index_page_keeps_the_page_indexed_when_confirmation_publish_fails() {
        // The words are already committed; losing the confirmation must not undo
        // that work or fail the job.
        let db = MockDB::with_page("<body>alpha</body>");
        let mq = MockMQ {
            publish_failures_remaining: AtomicUsize::new(99),
            ..Default::default()
        };
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());
        let page = indexer.db.page.lock().unwrap().clone().unwrap();

        let result = indexer.index_page(page).await;

        assert!(result.is_ok(), "publish failure must not fail indexing");
        assert_eq!(indexer.db.undo_calls.load(Ordering::SeqCst), 0);
        assert_eq!(indexer.db.batched_words.lock().unwrap().len(), 1);
    }

    // ── handler ──────────────────────────────────────────────────────────────

    #[tokio::test]
    async fn handler_indexes_a_well_formed_job() {
        let db = MockDB::with_page("<body>alpha</body>");
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());
        let page_id = indexer.db.page_id();

        let body = serde_json::to_vec(&Job { page_id }).unwrap();
        assert!(indexer.handler(body).await.is_ok());
        assert_eq!(indexer.db.batched_words.lock().unwrap().len(), 1);
    }

    #[tokio::test]
    async fn handler_rejects_a_malformed_payload() {
        let db = MockDB::with_page("<body>alpha</body>");
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());

        let err = indexer.handler(b"{not json".to_vec()).await.unwrap_err();

        assert!(
            matches!(err, AppError::SerdeError(_)),
            "a poison payload must be a serde error: {err:?}"
        );
        assert!(
            !should_requeue(&err),
            "a malformed payload must never be requeued or it loops forever"
        );
    }

    #[tokio::test]
    async fn handler_rejects_a_payload_with_a_non_uuid_page_id() {
        let db = MockDB::with_page("<body>alpha</body>");
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());

        let err = indexer
            .handler(br#"{"page_id":"definitely-not-a-uuid"}"#.to_vec())
            .await
            .unwrap_err();

        assert!(matches!(err, AppError::SerdeError(_)));
    }

    // ── requeue policy ───────────────────────────────────────────────────────

    #[test]
    fn requeue_policy_discards_poison_messages() {
        assert!(!should_requeue(&AppError::SerdeError(
            serde_json::from_slice::<Job>(b"nope").unwrap_err()
        )));
        assert!(!should_requeue(&AppError::ParseError("bad html".into())));
        assert!(!should_requeue(&AppError::Database(
            DatabaseError::NotFoundError("gone".into())
        )));
    }

    #[test]
    fn requeue_policy_retries_transient_failures() {
        assert!(should_requeue(&AppError::Database(
            DatabaseError::BatchWordsError("deadlock".into())
        )));
        assert!(should_requeue(&AppError::Database(
            DatabaseError::ConnectionError("db down".into())
        )));
        assert!(should_requeue(&AppError::Messaging(
            crate::core::errors::MessagingError::PublishError("nack".into())
        )));
        assert!(should_requeue(&AppError::Other("transient".into())));
    }

    #[test]
    fn requeue_policy_covers_every_error_variant() {
        // Guards against a new variant silently defaulting to the wrong branch.
        let every: Vec<AppError> = vec![
            AppError::Database(DatabaseError::SqlxError(sqlx::Error::PoolClosed)),
            AppError::Database(DatabaseError::ConnectionError("x".into())),
            AppError::Database(DatabaseError::QueryError("x".into())),
            AppError::Database(DatabaseError::NotFoundError("x".into())),
            AppError::Database(DatabaseError::BatchWordsError("x".into())),
            AppError::Messaging(crate::core::errors::MessagingError::ConnectionError(
                "x".into(),
            )),
            AppError::Messaging(crate::core::errors::MessagingError::ChannelError(
                "x".into(),
            )),
            AppError::Messaging(crate::core::errors::MessagingError::ConsumeError(
                "x".into(),
            )),
            AppError::Messaging(crate::core::errors::MessagingError::PublishError(
                "x".into(),
            )),
            AppError::Messaging(crate::core::errors::MessagingError::DeclareQueueError(
                "x".into(),
            )),
            AppError::SerdeError(serde_json::from_slice::<Job>(b"x").unwrap_err()),
            AppError::Other("x".into()),
            AppError::ParseError("x".into()),
        ];

        assert_eq!(every.len(), 13);
        for err in every {
            // Must not panic, and must return a definite answer.
            let _ = should_requeue(&err);
        }
    }

    // ── lifecycle ────────────────────────────────────────────────────────────

    #[tokio::test]
    async fn close_releases_both_the_queue_and_the_database() {
        let db = MockDB::with_page("<body>alpha</body>");
        let mq = MockMQ::default();
        let indexer = Indexer::new(db, mq, app_config(), discard_logger());

        indexer.close().await;

        assert_eq!(indexer.mq.closed.load(Ordering::SeqCst), 1);
        assert_eq!(indexer.db.closed.load(Ordering::SeqCst), 1);
    }

    #[tokio::test(start_paused = true)]
    async fn sweep_loop_republishes_stale_pages_and_stops_on_cancel() {
        let db = MockDB::with_page("<body>alpha</body>");
        let stale_id = Uuid::new_v4();
        db.stale.lock().unwrap().push(stale_id);

        let mq = MockMQ::default();
        let mut conf = app_config();
        conf.sweep_interval = Duration::from_secs(1);

        let indexer = Arc::new(Indexer::new(db, mq, conf, discard_logger()));
        let tk = CancellationToken::new();

        let handle = {
            let indexer = Arc::clone(&indexer);
            let tk = tk.clone();
            tokio::spawn(async move { indexer.sweep_loop(tk).await })
        };

        // Let one tick fire and the publish complete.
        tokio::time::sleep(Duration::from_millis(1_100)).await;
        tk.cancel();
        tokio::time::timeout(Duration::from_secs(2), handle)
            .await
            .expect("sweep_loop must stop promptly on cancellation")
            .unwrap();

        let published = indexer.mq.published.lock().unwrap();
        assert!(
            !published.is_empty(),
            "stale pages must be republished to the job queue"
        );
        for (queue, body) in published.iter() {
            assert_eq!(queue, "indexer.jobs");
            let job: Job = serde_json::from_slice(body).unwrap();
            assert!(!job.page_id.is_nil());
        }
        assert!(published.iter().any(|(_, b)| {
            serde_json::from_slice::<Job>(b)
                .map(|j| j.page_id == stale_id)
                .unwrap_or(false)
        }));
    }

    #[tokio::test(start_paused = true)]
    async fn sweep_loop_tolerates_a_failing_sweep_query() {
        let db = MockDB::with_page("<body>alpha</body>");
        let mq = MockMQ::default();
        let mut conf = app_config();
        conf.sweep_interval = Duration::from_secs(1);

        let indexer = Arc::new(Indexer::new(db, mq, conf, discard_logger()));
        let tk = CancellationToken::new();
        let handle = {
            let indexer = Arc::clone(&indexer);
            let tk = tk.clone();
            tokio::spawn(async move { indexer.sweep_loop(tk).await })
        };

        tokio::time::sleep(Duration::from_millis(1_100)).await;
        tk.cancel();
        tokio::time::timeout(Duration::from_secs(2), handle)
            .await
            .expect("an empty result must not stop the sweep loop")
            .unwrap();
    }

    #[tokio::test]
    async fn index_loop_refuses_to_consume_after_cancellation() {
        let db = MockDB::with_page("<body>alpha</body>");
        let mq = MockMQ::default();
        let indexer = Arc::new(Indexer::new(db, mq, app_config(), discard_logger()));

        let tk = CancellationToken::new();
        tk.cancel();

        let err = indexer.index_loop(tk).await.unwrap_err();

        assert!(
            matches!(err, AppError::Other(ref m) if m.contains("shutdown signal")),
            "unexpected error: {err:?}"
        );
    }
}
