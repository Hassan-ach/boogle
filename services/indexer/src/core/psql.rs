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
    /// Claims a page for indexing.
    ///
    /// `FOR UPDATE SKIP LOCKED` is what makes several indexer replicas safe: each
    /// takes a different unindexed row instead of blocking on the same one. The
    /// claim and the `indexed = TRUE` update share one statement so no row can be
    /// handed out without being marked.
    ///
    /// `indexed = TRUE` is set before indexing actually runs. A crash mid-index
    /// leaves the page marked, and `find_stale_pages` picks it back up via
    /// `updated_at`/`index_attempts` rather than leaving it unindexed forever.
    async fn get_page_by_id(&self, page_id: Uuid) -> Result<Page, AppError> {
        let mut tx = self.pool.begin().await?;
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

    /// Writes one page's word counts in a single multi-row upsert.
    ///
    /// One statement rather than a loop: a page with thousands of distinct words
    /// would otherwise cost thousands of round trips. Word ids come from an
    /// upsert of the words themselves, so a word never seen before is created in
    /// the same transaction.
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

        let mut keys: Vec<String> = storable.keys().map(|w| (*w).clone()).collect();
        keys.sort_unstable();

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

async fn db_connectioon(conf: &PsqlConfig) -> Result<Pool<Postgres>, AppError> {
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

async fn upsert_words(pool: &Pool<Postgres>, words: Vec<String>) -> Result<HashMap<String, Uuid>> {
    if words.is_empty() {
        return Ok(HashMap::new());
    }

    sqlx::query!(
        r#"INSERT INTO words (word)
           SELECT * FROM UNNEST($1::text[])
           ON CONFLICT (word) DO NOTHING"#,
        &words[..]
    )
    .execute(pool)
    .await?;

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

/// Longest word indexed, in characters. Longer tokens are dropped rather than
/// truncated, since a truncated word would be wrong and an untruncated one would
/// overflow the column. 25 covers English prose with room to spare.
const MAX_WORD_LEN: usize = 25;

/// Upserts word rows in chunks and returns each word's id.
///
/// Chunked because Postgres caps a statement at 65535 bind parameters, and a
/// page can easily have more distinct words than that.
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

    for chunk in words.chunks(batch_size) {
        match upsert_words(pool, chunk.to_vec()).await {
            Ok(chunk_ids) => {
                all_ids.extend(chunk_ids);
            }
            Err(err) => {
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

#[cfg(test)]
mod db_integration {
    use super::*;
    use slog::Drain;
    use std::env;

    fn test_logger() -> Logger {
        Logger::root(slog::Discard.fuse(), slog::o!())
    }

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

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_page_by_id_returns_the_html_and_claims_the_page() {
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
        let store = store_or_skip!();

        match store.get_page_by_id(Uuid::new_v4()).await {
            Err(AppError::Database(DatabaseError::NotFoundError(_))) => {}
            other => panic!("a random id resolved to a page: {other:?}"),
        }
    }

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

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn undo_indexing_returns_the_page_to_the_queue_and_counts_the_attempt() {
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
        let store = store_or_skip!();
        store
            .undo_indexing(Uuid::new_v4())
            .await
            .expect("undo_indexing on a missing page is a no-op");
    }

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn undo_indexing_does_not_touch_another_pages_attempt_count() {
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

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn get_stale_unindexed_reclaims_a_page_whose_worker_died() {
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

    #[tokio::test]
    #[ignore = "needs a live PostgreSQL; run with `cargo test -- --ignored`"]
    async fn connecting_to_a_database_that_is_not_there_fails_cleanly() {
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
