use crate::core::config::PsqlConfig;
use crate::core::indexer::Page;
use slog::{error, info, warn, Logger};
use std::collections::HashMap;
use std::error::Error;

use anyhow::Result;
use sqlx::{Pool, Postgres};
use uuid::Uuid;

pub trait DB {
    async fn get_page(&self) -> Result<Page>;
    async fn batch_words(&self, words: HashMap<String, u32>, page_id: Uuid) -> Result<()>;
}

#[derive(Debug, Clone)]
pub struct Psql {
    pub pool: Pool<Postgres>,
    pub conf: PsqlConfig,
    pub log: Logger,
}

impl Psql {
    pub async fn new(conf: PsqlConfig, log: Logger) -> Result<Self, Box<dyn Error>> {
        let pool = db_connectioon(&conf).await?;
        info!(log, "PostgreSQL connection pool created successfully";
             "max_connections" => conf.max_connections,
             "min_connections" => conf.min_connections,
             "acquire_timeout_seconds" => conf.acquire_timeout_seconds.as_secs()
        );
        Ok(Psql { pool, conf, log })
    }
}

impl DB for Psql {
    async fn get_page(&self) -> Result<Page> {
        let mut tx = self.pool.begin().await?;
        // Create a query type mapping
        let query = sqlx::query_as::<_, Page>(
            "WITH cte AS (
                 SELECT id, url_id, html
                 FROM pages
                 WHERE indexed = FALSE
                 FOR UPDATE SKIP LOCKED
                 LIMIT 1
            )
            UPDATE pages
            SET indexed = TRUE
            FROM cte
            WHERE pages.id = cte.id
            RETURNING pages.id, pages.url_id, pages.html",
        );
        // Fetch Optional row
        let page = query.fetch_one(&mut *tx).await?;
        tx.commit().await?;
        Ok(page)
    }
    async fn batch_words(&self, words: HashMap<String, u32>, page_id: Uuid) -> Result<()> {
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
                return Err(err);
            }
        };

        let word_id_count: HashMap<Uuid, u32> = map
            .into_iter()
            .filter_map(|(word, id)| words.get(&word).map(|count| (id, *count)))
            .collect();

        // this never gonna happen too.
        if let Err(err) = batch_link_words_to_page(
            &self.pool,
            page_id,
            word_id_count,
            self.conf.page_word_batch_size,
            &self.log,
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
}

// Function connect to postgres and test it
// return a pool connect
async fn db_connectioon(conf: &PsqlConfig) -> Result<Pool<Postgres>, Box<dyn Error>> {
    let pool = sqlx::postgres::PgPoolOptions::new()
        .max_connections(conf.max_connections)
        .min_connections(conf.min_connections)
        .acquire_timeout(conf.acquire_timeout_seconds)
        .connect(conf.url.as_str())
        .await?;

    let _ = sqlx::query("SELECT 1 + 1 as sum").fetch_one(&pool).await?;
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

async fn batch_link_words_to_page(
    pool: &Pool<Postgres>,
    page_id: Uuid,
    word_id_count: HashMap<Uuid, u32>,
    batch_size: usize,
    log: &Logger,
) -> Result<()> {
    if word_id_count.is_empty() {
        return Ok(());
    }

    let entries: Vec<_> = word_id_count.iter().collect();

    for chunk in entries.chunks(batch_size) {
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
            // Continue with next batch
        }
    }

    Ok(())
}
