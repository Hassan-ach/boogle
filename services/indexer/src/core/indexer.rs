use tokio::time;
use tokio::time::sleep;
use tokio::time::Duration;

use crate::core::config::AppConfig;
use crate::core::psql::DB;
use crate::core::text_sink::parse;
use slog::{error, info, Logger};
use sqlx::prelude::FromRow;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

#[derive(Debug, FromRow)]
pub struct Page {
    pub id: Uuid,
    pub url_id: Uuid,
    pub html: String,
}

#[derive(Debug, Clone)]
pub struct Indexer<DBImpl: DB> {
    db: DBImpl,
    conf: AppConfig,
    log: Logger,
}

pub trait Indexe {
    async fn start(&self, tk: CancellationToken);
}

impl<DBImpl: DB> Indexer<DBImpl> {
    pub fn new(db: DBImpl, conf: AppConfig, log: Logger) -> Self {
        Indexer { db, conf, log }
    }

    pub async fn index(&self, tk: &CancellationToken) {
        if tk.is_cancelled() {
            info!(
                self.log,
                "indexing task received shutdown signal, stopping..."
            );
            return;
        }
        match self.db.get_page().await {
            Ok(page) => {
                info!(self.log, "fetched page for indexing";
                     "page_id" => page.id.to_string(),
                     "url_id" => page.url_id.to_string(),
                     "html_size" => page.html.len()
                );

                match parse(page.html).await {
                    Ok(words) => {
                        info!(self.log, "parsed page for indexing";
                             "page_id" => page.id.to_string(),
                             "url_id" => page.url_id.to_string(),
                             "word_count" => words.len()
                        );
                        let _ = self.db.batch_words(words, page.id).await;
                    }
                    Err(err) => {
                        error!(self.log, "failed to parse page";
                               "page_id" => page.id.to_string(),
                               "url_id" => page.url_id.to_string(),
                               "error" => err.to_string()
                        );
                    }
                };
            }
            Err(err) => {
                error!(self.log, "failed to fetch page for indexing"; "error" => err.to_string());

                sleep(Duration::from_secs(5));
                // tk.cancel();
            }
        };
    }
}

impl<DBImpl: DB> Indexe for Indexer<DBImpl> {
    async fn start(&self, tk: CancellationToken) {
        loop {
            tokio::select! {
                _ = tk.cancelled() => {
                    info!(self.log, "indexing task received shutdown signal, stopping...");
                    break;
                }
                _ = async {
                    self.index(&tk).await;
                    sleep(Duration::from_millis(self.conf.loop_delay_ms)).await;
                } => {}
            }
        }
    }
}
