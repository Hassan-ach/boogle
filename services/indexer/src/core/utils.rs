use std::{fs::OpenOptions, io, path::Path, sync::Arc};

use slog::{Drain, Logger, o};
use slog_term::FullFormat;
use std::future::Future;

pub fn init_logger(log_file: &str) -> io::Result<Logger> {
    // Create log directory
    if let Some(parent) = Path::new(log_file).parent() {
        std::fs::create_dir_all(parent)?;
    }

    // Terminal: Pretty, colored output
    let term = slog_term::TermDecorator::new()
        .stderr()
        .force_color()
        .build();
    let term_drain = FullFormat::new(term).use_local_timestamp().build().fuse();

    // File: JSON format
    let file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(log_file)?;

    let json_drain = slog_json::Json::new(file)
        .add_default_keys()
        .add_key_value(slog::o!(
            "service" => "indexer",
        ))
        .build()
        .fuse();

    // Combine both - logs go to terminal AND file
    let drain = slog::Duplicate::new(term_drain, json_drain).fuse();

    // Wrap in Arc for thread-safe sharing across Tokio tasks
    // slog_async makes it non-blocking
    let async_drain = slog_async::Async::new(drain).chan_size(1024).build();

    Ok(Logger::root(Arc::new(async_drain).fuse(), o!()))
}

pub async fn retry_async<F, Fut, T, E>(max_retries: usize, mut f: F) -> Result<T, E>
where
    F: FnMut() -> Fut,
    Fut: Future<Output = Result<T, E>>,
{
    let mut attempts = 0;
    loop {
        match f().await {
            Ok(val) => return Ok(val),
            Err(err) => {
                attempts += 1;
                if attempts >= max_retries {
                    return Err(err);
                }
                tokio::time::sleep(std::time::Duration::from_millis(500)).await;
            }
        }
    }
}

pub fn retry_sync<F, T, E>(max_retries: usize, mut f: F) -> Result<T, E>
where
    F: FnMut() -> Result<T, E>,
{
    let mut attempts = 0;
    loop {
        match f() {
            Ok(val) => return Ok(val),
            Err(err) => {
                attempts += 1;
                if attempts >= max_retries {
                    return Err(err);
                }
                // Uses standard synchronous sleep
                std::thread::sleep(std::time::Duration::from_millis(500));
            }
        }
    }
}
