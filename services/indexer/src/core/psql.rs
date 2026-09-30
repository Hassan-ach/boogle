use crate::core::errors::{AppError, DatabaseError};
use crate::core::utils::retry_async;
use crate::core::{config::PsqlConfig, indexer::Page};
use slog::{Logger, error, info, warn};
use std::collections::HashMap;
use std::time::Duration;

use anyhow::Result;
use sqlx::postgres::PgConnectOptions;
use sqlx::{Pool, Postgres};
use uuid::Uuid;

#[async_trait::async_trait]
pub trait DB: Send + Sync {
    async fn get_page_by_id(&self, page_id: Uuid) -> Result<Page, AppError>;
    async fn batch_words(
        &self,
        words: &HashMap<String, u32>,
        page_id: Uuid,
    ) -> Result<(), AppError>;
    async fn undo_indexing(&self, page_id: Uuid) -> Result<(), AppError>;
    async fn close(&self);
    async fn get_stale_unindexed(
        &self,
        older_than: Duration,
        limit: i64,
    ) -> Result<Vec<Uuid>, AppError>;
}

#[derive(Debug, Clone)]
pub struct Psql {
    pub pool: Pool<Postgres>,
    pub conf: PsqlConfig,
    pub log: Logger,
}

impl Psql {
    pub async fn new(conf: PsqlConfig, log: Logger) -> Result<Self, AppError> {
        let pool = db_connectioon(&conf).await?;
        info!(log, "PostgreSQL connection pool created successfully";
             "max_connections" => conf.max_connections,
             "min_connections" => conf.min_connections,
             "acquire_timeout_seconds" => conf.acquire_timeout_seconds.as_secs(),
             "lock_timeout_ms" => conf.lock_timeout_ms,
             "statement_timeout_ms" => conf.statement_timeout_ms
        );
        Ok(Psql { pool, conf, log })
    }
}

#[async_trait::async_trait]
impl DB for Psql {
    async fn get_stale_unindexed(
        &self,
        older_than: Duration,
        limit: i64,
    ) -> Result<Vec<Uuid>, AppError> {
        // A page is marked indexed before any indexing work starts, so a worker that
        // died or hung leaves it claimed forever. Release stale claims first,
        // otherwise those pages are invisible to the sweep.
        //
        // Releasing also discards the words the dead worker managed to write.
        // They are a partial index of a page whose re-crawl may differ, and
        // leaving them in place means the retry's `DO UPDATE` refreshes the words
        // it sees while the ones it no longer has stay behind.
        sqlx::query!(
            r#"WITH released AS (
                    UPDATE pages
                    SET indexed = FALSE
                    WHERE indexed = TRUE
                        AND last_index_attempt_at IS NOT NULL
                        AND last_index_attempt_at < NOW() - make_interval(secs => $1)
                    RETURNING id
                )
                DELETE FROM page_word pw
                USING released
                WHERE pw.page_id = released.id"#,
            older_than.as_secs() as f64
        )
        .execute(&self.pool)
        .await?;

        let ids = sqlx::query_scalar!(
            r#"SELECT id FROM pages
                WHERE indexed = FALSE
                    AND updated_at < NOW() - make_interval(secs => $1)
                    AND (last_index_attempt_at IS NULL
                       OR last_index_attempt_at < NOW() - make_interval(secs => $1))
                    AND index_attempts < $3
                ORDER BY updated_at ASC
                LIMIT $2"#,
            older_than.as_secs() as f64,
            limit,
            3
        )
        .fetch_all(&self.pool)
        .await?;

        Ok(ids)
    }
    async fn get_page_by_id(&self, page_id: Uuid) -> Result<Page, AppError> {
        let mut tx = self.pool.begin().await?;
        // Create a query type mapping
        let query = sqlx::query_as::<_, Page>(
            "WITH cte AS (
                 SELECT id, url_id, html
                 FROM pages
                 WHERE indexed = FALSE
                    AND id = $1
                 FOR UPDATE SKIP LOCKED
                 LIMIT 1
            )
            UPDATE pages
            SET indexed = TRUE,
                last_index_attempt_at = NOW()
            FROM cte
            WHERE pages.id = cte.id
            RETURNING pages.id, pages.url_id, pages.html",
        )
        .bind(page_id);
        // Fetch Optional row
        let Some(page) = query.fetch_optional(&mut *tx).await? else {
            tx.commit().await?;
            return Err(AppError::Database(DatabaseError::NotFoundError(format!(
                "Page with id {} not found or already indexed",
                page_id
            ))));
        };
        tx.commit().await?;

        Ok(page)
    }

    async fn batch_words(
        &self,
        words: &HashMap<String, u32>,
        page_id: Uuid,
    ) -> Result<(), AppError> {
        if words.is_empty() {
            warn!(self.log, "no word to index for page";
                  "page_id" => page_id.to_string()
            );
            return Ok(());
        }

        // Drop what cannot be stored. `words.word` is VARCHAR(25) and Postgres
        // raises rather than truncating, so one long token would fail the entire
        // batch -- and the batch is every word on the page.
        let overlong: Vec<&str> = words
            .keys()
            .filter(|w| w.chars().count() > MAX_WORD_LEN)
            .map(|w| w.as_str())
            .collect();
        if !overlong.is_empty() {
            warn!(self.log, "dropping tokens too long for the words table";
                  "page_id" => page_id.to_string(),
                  "count" => overlong.len(),
                  "longest" => overlong.iter().map(|w| w.chars().count()).max().unwrap_or(0),
                  "example" => overlong.first().copied().unwrap_or("")
            );
        }
        let storable: HashMap<&String, u32> = words
            .iter()
            .filter(|(w, _)| w.chars().count() <= MAX_WORD_LEN)
            .map(|(w, c)| (w, *c))
            .collect();
        if storable.is_empty() {
            warn!(self.log, "every token on this page is too long to store";
                  "page_id" => page_id.to_string()
            );
            return Ok(());
        }

        // Sort so every concurrent worker inserts the same keys in the same order.
        // HashMap iteration order is randomized per task, which makes concurrent
        // INSERT ... ON CONFLICT speculative-insertion locks deadlock.
        let mut keys: Vec<String> = storable.keys().map(|w| (*w).clone()).collect();
        keys.sort_unstable();

        // Both halves of the write are now reported. A failure here used to be
        // logged and returned as `Ok(())`, and the caller's response to `Ok` is
        // to ack the job -- so the page stayed marked indexed with no words, it
        // never returned to the queue, and nothing above ERROR recorded why.
        let map = batch_upsert_words(&self.pool, keys, self.conf.word_batch_size, &self.log)
            .await
            .map_err(|err| {
                error!(self.log, "failed to upsert words for page";
                      "page_id" => page_id.to_string(),
                    "error" => %err
                );
                AppError::Database(DatabaseError::BatchWordsError(format!(
                    "failed to upsert words for page {}: {}",
                    page_id, err
                )))
            })?;

        let word_id_count: HashMap<Uuid, u32> = map
            .into_iter()
            .filter_map(|(word, id)| storable.get(&word).map(|count| (id, *count)))
            .collect();

        // An id set smaller than the word set means a word was in the batch but
        // came back without an id, which would index the page with a subset of
        // its words and call it a success.
        if word_id_count.len() != storable.len() {
            let err = format!(
                "resolved {} of {} word ids for page {}; the page would be indexed \
                 with only part of its content",
                word_id_count.len(),
                storable.len(),
                page_id
            );
            error!(self.log, "incomplete word upsert";
                  "page_id" => page_id.to_string(),
                  "error" => %err
            );
            return Err(AppError::Database(DatabaseError::BatchWordsError(err)));
        }

        // A page is only ever claimed while `indexed = FALSE`, and the only ways
        // out of that state are `undo_indexing` and the stale sweep -- both of
        // which drop the page's existing words. So by the time a word is written
        // the page has none, and the insert below is a replace rather than an
        // accumulate. `DO UPDATE` is still needed for the case where the page was
        // indexed, swept, and re-claimed within the same set.
        link_words_to_page(
            &self.pool,
            page_id,
            word_id_count,
            self.conf.page_word_batch_size,
            &self.log,
            self.conf.max_retries,
        )
        .await
    }

    async fn undo_indexing(&self, page_id: Uuid) -> Result<(), AppError> {
        // Drop whatever the attempt wrote. Un-marking the page is not enough on
        // its own: the next attempt upserts the words it finds now, and a word
        // that was removed from the page in the meantime is never deleted from
        // anywhere. It would go on contributing to the page's score for as long
        // as the row lived, which is forever.
        //
        // This and the `DO UPDATE` in `batch_link_words_to_page` are what make
        // indexing a page a replace rather than a merge. A page can only be
        // claimed while `indexed = FALSE`, and the only ways into that state are
        // here and the stale sweep, so every re-index passes through one of them.
        let removed = sqlx::query!("DELETE FROM page_word WHERE page_id = $1", page_id)
            .execute(&self.pool)
            .await?;
        if removed.rows_affected() > 0 {
            info!(self.log, "discarded the words of a failed indexing attempt";
                  "page_id" => page_id.to_string(),
                  "words" => removed.rows_affected()
            );
        }

        let result = sqlx::query!(
            "UPDATE pages
                    SET indexed = FALSE,
                    last_index_attempt_at = NOW(),
                    index_attempts = index_attempts + 1
                    WHERE id = $1",
            page_id
        )
        .execute(&self.pool)
        .await?;
        if result.rows_affected() == 0 {
            warn!(self.log, "no page found to undo indexing";
                  "page_id" => page_id.to_string()
            );
        } else {
            info!(self.log, "successfully undone indexing for page";
                  "page_id" => page_id.to_string()
            );
        }
        Ok(())
    }

    async fn close(&self) {
        self.pool.close().await;
        info!(self.log, "PostgreSQL connection pool closed successfully");
    }
}

// Function connect to postgres and test it
// return a pool connect
async fn db_connectioon(conf: &PsqlConfig) -> Result<Pool<Postgres>, AppError> {
    // Startup parameters, so they apply to every connection handed out by the pool.
    // Without these a lock wait is unbounded and a worker task can hang forever.
    let lock_timeout = format!("{}", conf.lock_timeout_ms);
    let statement_timeout = format!("{}", conf.statement_timeout_ms);
    let opts: PgConnectOptions = conf.url.parse()?;
    let opts = opts.options([
        ("application_name", "boogle-indexer"),
        ("lock_timeout", lock_timeout.as_str()),
        ("statement_timeout", statement_timeout.as_str()),
        ("idle_in_transaction_session_timeout", "30000"),
    ]);

    let pool = match sqlx::postgres::PgPoolOptions::new()
        .max_connections(conf.max_connections)
        .min_connections(conf.min_connections)
        .acquire_timeout(conf.acquire_timeout_seconds)
        .connect_with(opts)
        .await
    {
        Ok(pool) => pool,
        Err(err) => {
            return Err(AppError::Database(DatabaseError::ConnectionError(format!(
                "failed to connect to database: {}",
                err
            ))));
        }
    };

    let _ = match sqlx::query("SELECT 1 + 1 as sum").fetch_one(&pool).await {
        Ok(_) => {}
        Err(err) => {
            return Err(AppError::Database(DatabaseError::QueryError(format!(
                "failed to test database connection: {}",
                err
            ))));
        }
    };

    println!("Data base connected successfully");
    Ok(pool)
}

// Function to insert words in batch and return their ids
async fn upsert_words(pool: &Pool<Postgres>, words: Vec<String>) -> Result<HashMap<String, Uuid>> {
    if words.is_empty() {
        return Ok(HashMap::new());
    }

    // Using UNNEST to pass the entire vector as one parameter ($1)
    //
    // Two statements, because the obvious one-statement form was the single worst
    // bug in this service. It used to be:
    //
    //     WITH inserted AS (
    //         INSERT INTO words (word) SELECT * FROM UNNEST($1::text[])
    //         ON CONFLICT (word) DO NOTHING
    //     )
    //     SELECT words.id, words.word FROM words
    //     INNER JOIN UNNEST($1::text[]) u(word) ON words.word = u.word
    //
    // which reads as "insert the words, then look them up". But every
    // sub-statement in a WITH sees the *same* snapshot as the main query, so the
    // trailing SELECT could not see anything `inserted` had just written. It only
    // ever found words that were already in the table -- and a word that is
    // already in the table is, by definition, a word some earlier page put there.
    //
    // The consequences compounded. `upsert_words` returned nothing for new words,
    // so `word_id_count` was empty, so `link_words_to_page` returned early, so
    // `batch_words` reported success having written no rows. The job was acked and
    // the page marked indexed. A brand new word was therefore dropped on the floor
    // unless the same page happened to be crawled again later -- so on a fresh
    // index every page had to be visited twice before it contributed anything, and
    // a page visited once was invisible to search for good. Nothing was logged
    // above ERROR anywhere along that chain.
    //
    // The single-statement alternative is `ON CONFLICT DO UPDATE ... RETURNING`,
    // which does return every input word. It is not used here because it takes a
    // row lock on every word that already exists, and the common words in a crawl
    // are exactly the ones every worker wants at the same time. `DO NOTHING` plus
    // a separate read keeps concurrent workers from serialising on each other.
    sqlx::query!(
        r#"INSERT INTO words (word)
           SELECT * FROM UNNEST($1::text[])
           ON CONFLICT (word) DO NOTHING"#,
        &words[..]
    )
    .execute(pool)
    .await?;

    // Every requested word exists by now: it was either already there or the
    // statement above put it there. `words.word` is UNIQUE, so this returns
    // exactly one row per input and the join is redundant.
    let rows = sqlx::query!(
        r#"SELECT id, word FROM words WHERE word = ANY($1)"#,
        &words[..]
    )
    .fetch_all(pool)
    .await?;

    let mut ids = HashMap::with_capacity(rows.len());
    for row in rows {
        ids.insert(row.word, row.id);
    }

    Ok(ids)
}

/// `words.word` is `VARCHAR(25)`. A longer value is an error, not a truncation,
/// so it has to be filtered before it reaches the statement.
///
/// This is not a theoretical bound: real pages are full of long unbroken
/// alphabetic runs -- minified JavaScript identifiers, CSS class names, base64
/// blobs, long URL path segments. Since the upsert takes the whole batch as one
/// statement, a single 26-character token on an otherwise ordinary page fails
/// every word on that page, and the error used to be swallowed, so the page was
/// acked with nothing indexed.
///
/// The limit is named rather than inlined so it reads as a schema fact and not a
/// tuning knob; it mirrors `migration/01_schema.sql`.
const MAX_WORD_LEN: usize = 25;

async fn batch_upsert_words(
    pool: &Pool<Postgres>,
    words: Vec<String>,
    batch_size: usize,
    log: &Logger,
) -> Result<HashMap<String, Uuid>> {
    if words.is_empty() {
        return Ok(HashMap::new());
    }

    let mut all_ids = HashMap::with_capacity(words.len());

    // Process words in chunks
    for chunk in words.chunks(batch_size) {
        match upsert_words(pool, chunk.to_vec()).await {
            Ok(chunk_ids) => {
                all_ids.extend(chunk_ids);
            }
            Err(err) => {
                // Returning the first failure rather than continuing is the point.
                // Carrying on produced an id map missing the whole chunk, which
                // `batch_words` then linked as a partial page, and the missing-id
                // check above could not tell that apart from a filter decision.
                error!(log, "failed to upsert batch of words";
                      "batch_size" => chunk.len(),
                      "error" => %err
                );
                return Err(err);
            }
        }
    }

    Ok(all_ids)
}

async fn link_words_to_page(
    pool: &Pool<Postgres>,
    page_id: Uuid,
    word_id_count: HashMap<Uuid, u32>,
    batch_size: usize,
    log: &Logger,
    max_retries: usize,
) -> Result<(), AppError> {
    if word_id_count.is_empty() {
        return Ok(());
    }

    // Same ordering guarantee as the word upsert, so concurrent workers never
    // grab page_word locks in conflicting orders.
    let mut entries: Vec<_> = word_id_count.iter().collect();
    entries.sort_unstable_by_key(|(id, _)| **id);

    for chunk in entries.chunks(batch_size) {
        match retry_async(max_retries, || async {
            batch_link_words_to_page(pool, page_id, chunk, log).await
        })
        .await
        {
            Err(err) => {
                error!(log, "failed to link batch of words to page after retries";
                      "batch_size" => chunk.len(),
                      "error" => %err
                );
                return Err(AppError::Database(DatabaseError::BatchWordsError(format!(
                    "failed to link batch of words to page {} after retries: {}",
                    page_id, err
                ))));
            }
            Ok(_) => {
                info!(log, "successfully linked batch of words to page";
                      "batch_size" => chunk.len(),
                      "page_id" => page_id.to_string()
                );
            }
        }
    }

    Ok(())
}

async fn batch_link_words_to_page(
    pool: &Pool<Postgres>,
    page_id: Uuid,
    chunk: &[(&Uuid, &u32)],
    log: &Logger,
) -> Result<(), AppError> {
    let mut word_ids = Vec::with_capacity(chunk.len());
    let mut counts = Vec::with_capacity(chunk.len());

    for (id, count) in chunk {
        word_ids.push(**id);
        counts.push((**count) as i32);
    }

    let result = sqlx::query!(
        r#"
            INSERT INTO page_word (page_id, word_id, tf)
            SELECT $1, * FROM UNNEST($2::uuid[], $3::int4[])
            ON CONFLICT (page_id, word_id) DO UPDATE SET tf = EXCLUDED.tf
            "#,
        page_id,
        &word_ids,
        &counts
    )
    .execute(pool)
    .await;

    if let Err(err) = result {
        error!(log, "failed to link batch of words to page";
              "batch_size" => chunk.len(),
              "error" => %err
        );
        return Err(AppError::Database(DatabaseError::BatchWordsError(format!(
            "failed to link batch of words to page {}: {}",
            page_id, err
        ))));
    }
    return Ok(());
}

/// Integration tests against a live PostgreSQL.
///
/// Every test here is `#[ignore]`d and runs only under `cargo test -- --ignored`,
/// which `just test-integration` does for you. They need `DATABASE_URL` to point
/// at the throwaway `boogle_test` database that `scripts/setup-test-db.sh`
/// creates -- never the live index, because these tests write and delete rows.
///
/// `MockDB` in `indexer.rs` covers the *policy* around this trait: which jobs get
/// requeued, when a claim is released, how many attempts a page gets. It cannot
/// cover the *SQL*, and the SQL is where the interesting failures live -- a
/// column renamed in `01_schema.sql`, a conflict clause that quietly keeps stale
/// values, an error that gets logged and dropped. A mock that agrees with broken
/// SQL is worse than no test, because it is confidently green.
///
/// Every test seeds under a `https://it-<name>-<uuid>` URL and deletes it
/// afterwards, so the tests are order-independent and can be run in parallel.
#[cfg(test)]
mod db_integration {
    use super::*;
    use slog::Drain;
    use std::env;

    /// A logger that discards everything, so the tests do not spew slog output.
    fn test_logger() -> Logger {
        Logger::root(slog::Discard.fuse(), slog::o!())
    }

    /// The DSN to test against, or `None` to skip the test.
    ///
    /// Skipping rather than failing is deliberate: `cargo test -- --ignored` on a
    /// machine with no database should be a clean no-op, not a wall of red that
    /// hides real failures.
    fn dsn() -> Option<String> {
        match env::var("DATABASE_URL") {
            Ok(url) if !url.is_empty() => Some(url),
            _ => {
                eprintln!("DATABASE_URL is unset; skipping a database integration test");
                None
            }
        }
    }

    fn conf(url: &str) -> PsqlConfig {
        PsqlConfig {
            url: url.to_string(),
            max_connections: 4,
            min_connections: 0,
            acquire_timeout_seconds: Duration::from_secs(10),
            lock_timeout_ms: 5_000,
            statement_timeout_ms: 15_000,
            word_batch_size: 100,
            page_word_batch_size: 100,
            max_retries: 3,
        }
    }

    /// Connect, or skip the calling test.
    macro_rules! store_or_skip {
        () => {
            match dsn() {
                Some(url) => Psql::new(conf(&url), test_logger())
                    .await
                    .expect("could not connect to the test database"),
                None => return,
            }
        };
    }

    /// Seed an unindexed page and return its id.
    ///
    /// `updated_at` is pushed into the past so the page is old enough for
    /// `get_stale_unindexed`, which filters on it.
    async fn seed_page(store: &Psql, label: &str) -> (Uuid, Uuid) {
        let url = format!("https://it-{label}-{}/", Uuid::new_v4());
        let url_id: Uuid = sqlx::query_scalar("INSERT INTO urls (url) VALUES ($1) RETURNING id")
            .bind(&url)
            .fetch_one(&store.pool)
            .await
            .expect("insert url");
        let page_id: Uuid = sqlx::query_scalar(
            "INSERT INTO pages (url_id, html, indexed, updated_at) \
             VALUES ($1, $2, FALSE, NOW() - INTERVAL '1 day') RETURNING id",
        )
        .bind(url_id)
        .bind("<html><body>hello indexed world</body></html>")
        .fetch_one(&store.pool)
        .await
        .expect("insert page");
        (url_id, page_id)
    }

    /// Remove everything a seeded page owns. Cascades handle pages and page_word.
    async fn cleanup(store: &Psql, url_id: Uuid) {
        sqlx::query("DELETE FROM urls WHERE id = $1")
            .bind(url_id)
            .execute(&store.pool)
            .await
            .expect("clean up the seeded url");
    }

    fn words(pairs: &[(&str, u32)]) -> HashMap<String, u32> {
        pairs.iter().map(|(w, c)| (w.to_string(), *c)).collect()
    }

    // ── get_page_by_id ───────────────────────────────────────────────────────

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_page_by_id_returns_the_html_and_claims_the_page() {
        // Claiming is the whole point of this call: the page is flipped to
        // indexed with a timestamp so no other worker picks it up, and
        // get_stale_unindexed will release it if this worker dies. A
        // `get_page_by_id` that returned the page without claiming it would let
        // every worker in the pool index the same page.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "claim").await;

        let page = store.get_page_by_id(page_id).await.expect("get_page_by_id");

        assert_eq!(page.id, page_id);
        assert_eq!(page.url_id, url_id);
        assert!(page.html.contains("hello indexed world"));

        let (indexed, claimed): (bool, bool) = sqlx::query_as(
            "SELECT indexed, last_index_attempt_at IS NOT NULL FROM pages WHERE id = $1",
        )
        .bind(page_id)
        .fetch_one(&store.pool)
        .await
        .expect("read the claim back");
        assert!(indexed, "the page was returned but not claimed");
        assert!(claimed, "claimed without a timestamp");

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_page_by_id_reports_a_page_it_already_claimed_as_not_found() {
        // The second call is what a duplicate delivery of the same RabbitMQ
        // message looks like. It has to be a NotFound rather than a second
        // successful claim, or the same page gets indexed twice concurrently.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "dup").await;

        store.get_page_by_id(page_id).await.expect("first claim");

        let second = store.get_page_by_id(page_id).await;
        match second {
            Err(AppError::Database(DatabaseError::NotFoundError(_))) => {}
            other => panic!("a second claim succeeded: {other:?}"),
        }

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_page_by_id_reports_a_random_id_as_not_found() {
        // The caller requeues on any error, so a missing page has to arrive as
        // an Err. Returning a zero-value Page here would index an empty
        // document under a real page id.
        let store = store_or_skip!();

        match store.get_page_by_id(Uuid::new_v4()).await {
            Err(AppError::Database(DatabaseError::NotFoundError(_))) => {}
            other => panic!("a random id resolved to a page: {other:?}"),
        }
    }

    // ── batch_words ──────────────────────────────────────────────────────────

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn batch_words_stores_the_term_frequencies_it_was_given() {
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "batch").await;

        store
            .batch_words(&words(&[("alpha", 3), ("beta", 1)]), page_id)
            .await
            .expect("batch_words");

        let rows: Vec<(String, i32)> = sqlx::query_as(
            "SELECT w.word, pw.tf FROM page_word pw JOIN words w ON w.id = pw.word_id \
             WHERE pw.page_id = $1 ORDER BY w.word",
        )
        .bind(page_id)
        .fetch_all(&store.pool)
        .await
        .expect("read page_word back");

        assert_eq!(
            rows,
            vec![("alpha".to_string(), 3), ("beta".to_string(), 1)],
        );

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn batch_words_on_a_recrawled_page_replaces_the_old_term_frequencies() {
        // Re-crawling is routine, and the HTML often changed in the meantime. The
        // insert used to be `ON CONFLICT (page_id, word_id) DO NOTHING`, which
        // silently kept the term frequency from the first crawl forever: a word
        // that went from 5 occurrences to 9 stayed at 5, and a word that was
        // deleted from the page kept contributing to its score forever. Nothing
        // errored; the ranking was just quietly wrong for as long as the page lived.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "recrawl").await;

        store
            .batch_words(&words(&[("grow", 5), ("gone", 2)]), page_id)
            .await
            .expect("first crawl");
        store
            .batch_words(&words(&[("grow", 9), ("fresh", 4)]), page_id)
            .await
            .expect("second crawl");

        let rows: Vec<(String, i32)> = sqlx::query_as(
            "SELECT w.word, pw.tf FROM page_word pw JOIN words w ON w.id = pw.word_id \
             WHERE pw.page_id = $1 ORDER BY w.word",
        )
        .bind(page_id)
        .fetch_all(&store.pool)
        .await
        .expect("read page_word back");

        let got: std::collections::HashMap<&str, i32> =
            rows.iter().map(|(w, tf)| (w.as_str(), *tf)).collect();

        assert_eq!(
            got.get("grow").copied(),
            Some(9),
            "a re-crawled page kept the term frequency from its first crawl: {rows:?}"
        );
        assert_eq!(
            got.get("fresh").copied(),
            Some(4),
            "a word added by the re-crawl is missing: {rows:?}"
        );

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn batch_words_reports_a_failed_link_instead_of_swallowing_it() {
        // `link_words_to_page` failing used to be logged and then turned into
        // `Ok(())`. The caller in `indexer.rs` only requeues on an error, so a
        // swallowed failure meant the job was acked, the page stayed marked
        // indexed, and its words were never written -- permanently invisible to
        // search, with nothing in the logs above ERROR to say why.
        //
        // A page id that does not exist makes the page_word insert fail its
        // foreign key, which is the smallest way to reach that branch.
        let store = store_or_skip!();

        let result = store
            .batch_words(&words(&[("orphan", 1)]), Uuid::new_v4())
            .await;

        assert!(
            result.is_err(),
            "batch_words reported success for a page that does not exist, so the \
             caller acked the job and the page stayed marked indexed with no words"
        );
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn batch_words_with_nothing_to_index_is_a_no_op() {
        // A page whose HTML is nothing but script and style tokenizes to nothing.
        // That must be a success, not an error, or every JS-only page is requeued
        // forever.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "empty").await;

        store
            .batch_words(&HashMap::new(), page_id)
            .await
            .expect("an empty word set is not a failure");

        let remaining: i64 =
            sqlx::query_scalar("SELECT count(*) FROM page_word WHERE page_id = $1")
                .bind(page_id)
                .fetch_one(&store.pool)
                .await
                .expect("count");
        assert_eq!(remaining, 0);

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn batch_words_drops_a_token_too_long_for_the_column_instead_of_failing() {
        // `words.word` is VARCHAR(25) and Postgres raises on a longer value rather
        // than truncating, so a single long token used to fail the entire upsert
        // batch -- which is every word on the page. Real pages are full of long
        // unbroken alphabetic runs: minified JavaScript, CSS class names, base64
        // blobs, long path segments. Any of them made the whole page silently
        // unindexable, because the error was swallowed further up.
        //
        // The token is dropped and the rest of the page is indexed. Truncating
        // instead would merge distinct long words onto one key, which is worse
        // than losing the one.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "longtoken").await;

        let too_long = "a".repeat(40);
        store
            .batch_words(
                &words(&[("ordinary", 2), (too_long.as_str(), 1), ("alsonormal", 1)]),
                page_id,
            )
            .await
            .expect("one long token must not fail the page");

        let rows: Vec<(String, i32)> = sqlx::query_as(
            "SELECT w.word, pw.tf FROM page_word pw JOIN words w ON w.id = pw.word_id \
             WHERE pw.page_id = $1 ORDER BY w.word",
        )
        .bind(page_id)
        .fetch_all(&store.pool)
        .await
        .expect("read page_word back");

        let got: Vec<&str> = rows.iter().map(|(w, _)| w.as_str()).collect();
        assert_eq!(
            got,
            vec!["alsonormal", "ordinary"],
            "the long token should be dropped and the rest of the page kept: {rows:?}"
        );

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn batch_words_spans_more_rows_than_the_batch_size_holds() {
        // The batching is there to keep the parameter list under Postgres's
        // 65535 limit. Words whose counts are chunked across several batches must
        // all be linked, and the chunking is invisible from the call site.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "chunked").await;

        let many: Vec<(String, u32)> = (0..25)
            .map(|i| (format!("chunkword{i}"), (i % 7) + 1))
            .collect();
        let count = many.len() as u32;

        store
            .batch_words(&many.into_iter().collect(), page_id)
            .await
            .expect("batch_words");

        let (rows,): (i64,) = sqlx::query_as("SELECT count(*) FROM page_word WHERE page_id = $1")
            .bind(page_id)
            .fetch_one(&store.pool)
            .await
            .expect("count");
        assert_eq!(rows, i64::from(count));

        cleanup(&store, url_id).await;
    }

    // ── undo_indexing ────────────────────────────────────────────────────────

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn undo_indexing_returns_the_page_to_the_queue_and_counts_the_attempt() {
        // The whole retry contract rests on this: a page whose indexing failed
        // has to go back to being unindexed, and the attempt has to be recorded
        // so `get_stale_unindexed` eventually gives up on it.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "undo").await;

        sqlx::query("UPDATE pages SET indexed = TRUE WHERE id = $1")
            .bind(page_id)
            .execute(&store.pool)
            .await
            .expect("mark indexed");

        store.undo_indexing(page_id).await.expect("undo_indexing");

        let (indexed, attempts): (bool, i32) =
            sqlx::query_as("SELECT indexed, index_attempts FROM pages WHERE id = $1")
                .bind(page_id)
                .fetch_one(&store.pool)
                .await
                .expect("read the page back");
        assert!(
            !indexed,
            "the page is still claimed, so the sweep will never see it"
        );
        assert_eq!(attempts, 1, "the failed attempt was not recorded");

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn undo_indexing_discards_the_words_the_failed_attempt_wrote() {
        // Un-marking the page is only half of undoing an indexing. A page is
        // re-indexed whenever it is re-crawled, and the content usually changed in
        // between. The words this attempt wrote are a partial index of a version
        // of the page that no longer exists; leaving them means the retry's
        // `DO UPDATE` refreshes the words it still sees while the ones the page
        // no longer contains stay behind, scoring the page on content that was
        // deleted weeks ago.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "undo-words").await;

        store
            .batch_words(&words(&[("kept", 1), ("removed", 1)]), page_id)
            .await
            .expect("batch_words");

        store.undo_indexing(page_id).await.expect("undo_indexing");

        let remaining: Vec<String> = sqlx::query_scalar(
            "SELECT w.word FROM page_word pw JOIN words w ON w.id = pw.word_id \
             WHERE pw.page_id = $1 ORDER BY w.word",
        )
        .bind(page_id)
        .fetch_all(&store.pool)
        .await
        .expect("read page_word back");

        assert!(
            remaining.is_empty(),
            "undo_indexing left the failed attempt's words behind: {remaining:?}"
        );

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn undo_indexing_is_safe_on_a_page_that_does_not_exist() {
        // The rollback path calls this after a failure, and the failure may have
        // been the page disappearing. It must not raise a second error on top of
        // the first, or the original cause is lost behind "no page found".
        let store = store_or_skip!();
        store
            .undo_indexing(Uuid::new_v4())
            .await
            .expect("undo_indexing on a missing page is a no-op");
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn undo_indexing_does_not_touch_another_pages_attempt_count() {
        // A missing `WHERE id = $1` would reset every page's attempts and hand
        // the whole failed backlog back to the workers at once.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "undo-a").await;
        let (other_url_id, other_page_id) = seed_page(&store, "undo-b").await;

        for id in [page_id, other_page_id] {
            sqlx::query("UPDATE pages SET indexed = TRUE, index_attempts = 2 WHERE id = $1")
                .bind(id)
                .execute(&store.pool)
                .await
                .expect("seed attempts");
        }

        store.undo_indexing(page_id).await.expect("undo_indexing");

        let attempts: i32 = sqlx::query_scalar("SELECT index_attempts FROM pages WHERE id = $1")
            .bind(other_page_id)
            .fetch_one(&store.pool)
            .await
            .expect("read the untouched page");
        assert_eq!(
            attempts, 2,
            "undoing one page moved another page's attempt count"
        );

        cleanup(&store, url_id).await;
        cleanup(&store, other_url_id).await;
    }

    // ── get_stale_unindexed ──────────────────────────────────────────────────

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_stale_unindexed_reclaims_a_page_whose_worker_died() {
        // A page is claimed by get_page_by_id before any work starts. If the
        // worker is killed -- OOM, deploy, panic -- the claim is never released
        // and the page is invisible to search forever. The sweep is the only
        // thing that brings it back.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "stale").await;

        sqlx::query(
            "UPDATE pages SET indexed = TRUE, last_index_attempt_at = NOW() - INTERVAL '2 hours' \
             WHERE id = $1",
        )
        .bind(page_id)
        .execute(&store.pool)
        .await
        .expect("age the claim");

        let found = store
            .get_stale_unindexed(Duration::from_secs(3600), 100)
            .await
            .expect("get_stale_unindexed");

        assert!(
            found.contains(&page_id),
            "the two-hour-old claim was never reclaimed: {found:?}"
        );
        let still_claimed: bool = sqlx::query_scalar("SELECT indexed FROM pages WHERE id = $1")
            .bind(page_id)
            .fetch_one(&store.pool)
            .await
            .expect("read indexed");
        assert!(!still_claimed, "the page was returned but not released");

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_stale_unindexed_leaves_a_worker_that_is_still_working_alone() {
        // The mirror image of the test above, and the reason the sweep takes an
        // age at all: a page claimed seconds ago belongs to a worker that is
        // still indexing it. Releasing it hands the same page to a second
        // worker, and the two race to write the same page_word rows.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "fresh").await;

        sqlx::query(
            "UPDATE pages SET indexed = TRUE, last_index_attempt_at = NOW() - INTERVAL '1 minute' \
             WHERE id = $1",
        )
        .bind(page_id)
        .execute(&store.pool)
        .await
        .expect("mark a recent claim");

        let found = store
            .get_stale_unindexed(Duration::from_secs(3600), 100)
            .await
            .expect("get_stale_unindexed");

        assert!(
            !found.contains(&page_id),
            "a claim taken a minute ago was pulled out from under its worker: {found:?}"
        );
        let still_claimed: bool = sqlx::query_scalar("SELECT indexed FROM pages WHERE id = $1")
            .bind(page_id)
            .fetch_one(&store.pool)
            .await
            .expect("read indexed");
        assert!(still_claimed);

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_stale_unindexed_gives_up_on_a_page_after_three_failed_attempts() {
        // Without a ceiling, a page that reliably fails -- a 500, a login wall,
        // a malformed body -- is picked up by every sweep forever, and the
        // backlog never drains. The ceiling is 3, hard-coded in the query.
        let store = store_or_skip!();
        let (url_id, page_id) = seed_page(&store, "gave-up").await;

        sqlx::query("UPDATE pages SET index_attempts = 3 WHERE id = $1")
            .bind(page_id)
            .execute(&store.pool)
            .await
            .expect("exhaust the attempts");

        let found = store
            .get_stale_unindexed(Duration::from_secs(3600), 100)
            .await
            .expect("get_stale_unindexed");

        assert!(
            !found.contains(&page_id),
            "a page that already failed three times was handed out again: {found:?}"
        );

        cleanup(&store, url_id).await;
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_stale_unindexed_honours_its_limit() {
        // The limit is the sweep's only brake. Ignoring it would pull the entire
        // backlog into memory at once.
        let store = store_or_skip!();

        let mut seeded = Vec::new();
        for _ in 0..4 {
            let (url_id, page_id) = seed_page(&store, "limit").await;
            seeded.push((url_id, page_id));
        }

        let found = store
            .get_stale_unindexed(Duration::from_secs(3600), 2)
            .await
            .expect("get_stale_unindexed");
        assert!(found.len() <= 2, "a limit of 2 returned {}", found.len());

        for (url_id, _) in seeded {
            cleanup(&store, url_id).await;
        }
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_stale_unindexed_returns_the_oldest_pages_first() {
        // Ordering is what makes the sweep drain a backlog rather than spin on
        // the same few pages: the oldest unindexed page is the one most likely
        // to be sitting in the queue unclaimed.
        let store = store_or_skip!();

        let (older_url, older) = seed_page(&store, "older").await;
        sqlx::query("UPDATE pages SET updated_at = NOW() - INTERVAL '3 days' WHERE id = $1")
            .bind(older)
            .execute(&store.pool)
            .await
            .expect("age the older page");

        let (newer_url, newer) = seed_page(&store, "newer").await;
        sqlx::query("UPDATE pages SET updated_at = NOW() - INTERVAL '2 days' WHERE id = $1")
            .bind(newer)
            .execute(&store.pool)
            .await
            .expect("age the newer page");

        let found = store
            .get_stale_unindexed(Duration::from_secs(3600), 100)
            .await
            .expect("get_stale_unindexed");

        let older_at = found.iter().position(|id| *id == older);
        let newer_at = found.iter().position(|id| *id == newer);
        match (older_at, newer_at) {
            (Some(o), Some(n)) => assert!(o < n, "the newer page was handed out first"),
            _ => panic!("one of the seeded pages was missing from the sweep: {found:?}"),
        }

        cleanup(&store, older_url).await;
        cleanup(&store, newer_url).await;
    }

    // ── connection lifecycle ─────────────────────────────────────────────────

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn connecting_to_a_database_that_is_not_there_fails_cleanly() {
        // A bad DSN is an operator error, and it should surface as an AppError
        // naming the connection rather than a panic from inside sqlx.
        let bogus = match dsn() {
            Some(url) => url,
            None => return,
        };
        let mangled = bogus.replace("@", "@no-such-host-9d3f@");

        match Psql::new(conf(&mangled), test_logger()).await {
            Err(AppError::Database(_)) => {}
            other => panic!("connecting to a bogus host did not fail cleanly: {other:?}"),
        }
    }
}
