use crate::core::errors::{AppError, DatabaseError};
use crate::core::utils::retry_async;
use crate::core::{config::PsqlConfig, indexer::Page};
use slog::{Logger, error, info, warn};
use std::collections::HashMap;
use std::time::Duration;

use anyhow::Result;
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
             "acquire_timeout_seconds" => conf.acquire_timeout_seconds.as_secs()
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
            SET indexed = TRUE
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

        let map = match batch_upsert_words(
            &self.pool,
            words.clone().into_keys().collect(),
            self.conf.word_batch_size,
            &self.log,
        )
        .await
        {
            Ok(m) => m,

            // this never gonna happen
            Err(err) => {
                error!(self.log, "failed to upsert words for page";
                      "page_id" => page_id.to_string(),
                    "error" => %err
                );
                return Err(AppError::Database(DatabaseError::BatchWordsError(format!(
                    "failed to upsert words for page {}: {}",
                    page_id, err
                ))));
            }
        };

        let word_id_count: HashMap<Uuid, u32> = map
            .into_iter()
            .filter_map(|(word, id)| words.get(&word).map(|count| (id, *count)))
            .collect();

        // this should never happen.
        if let Err(err) = link_words_to_page(
            &self.pool,
            page_id,
            word_id_count,
            self.conf.page_word_batch_size,
            &self.log,
            self.conf.max_retries,
        )
        .await
        {
            error!(self.log, "failed to link words to page";
                  "page_id" => page_id.to_string(),
                  "error" => %err
            );
        }

        Ok(())
    }

    async fn undo_indexing(&self, page_id: Uuid) -> Result<(), AppError> {
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
    let pool = match sqlx::postgres::PgPoolOptions::new()
        .max_connections(conf.max_connections)
        .min_connections(conf.min_connections)
        .acquire_timeout(conf.acquire_timeout_seconds)
        .connect(conf.url.as_str())
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
    let rows = sqlx::query!(
        r#"
        WITH inserted AS (
            INSERT INTO words (word)
            SELECT * FROM UNNEST($1::text[])
            ON CONFLICT (word) DO NOTHING
        )
        SELECT words.id, words.word FROM words
        INNER JOIN UNNEST($1::text[]) u(word)
        ON words.word = u.word
        "#,
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
                error!(log, "failed to upsert batch of words";
                      "batch_size" => chunk.len(),
                      "error" => %err
                );
                // Continue with next batch instead of failing completely
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

    let entries: Vec<_> = word_id_count.iter().collect();

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
            ON CONFLICT (page_id, word_id) DO NOTHING
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
