mod core;

use crate::core::config::load_config;
use crate::core::errors::AppError;
use crate::core::indexer::Indexe;
use crate::core::indexer::Indexer;
use crate::core::messaging::RabbitMQ;
use crate::core::psql::Psql;
use crate::core::utils::init_logger;
use slog::error;
use slog::info;
use std::process;
use std::sync::Arc;
use tokio::time::timeout;

#[tokio::main]
async fn main() -> Result<(), AppError> {
    let conf = load_config(".env".to_string());
    let log = init_logger(conf.app.log_path.clone().as_str())?;

    let psql = Psql::new(conf.psql, log.clone()).await?;
    let mq = RabbitMQ::new(conf.rabbit, log.clone()).await?;
    let indx = Arc::new(Indexer::<Psql, RabbitMQ>::new(
        psql,
        mq,
        conf.app,
        log.clone(),
    ));

    let token = tokio_util::sync::CancellationToken::new();

    // Start the indexer loop in the background
    let indexer_handle = tokio::spawn(Arc::clone(&indx).start(token.clone()));

    // Start the sweep loop in the background
    let sweep_handle = tokio::spawn(Arc::clone(&indx).sweep_loop(token.clone()));

    // Spawn a task to listen for Ctrl-C signal and cancel the token when received
    let log_clone = log.clone();
    let token_clone = token.clone();
    let sig_handle = tokio::spawn(async move {
        if let Err(err) = tokio::signal::ctrl_c().await {
            error!(log_clone, "Failed to listen for Ctrl-C"; "error" => %err);
        } else {
            info!(log_clone, "Ctrl-C received, sending shutdown signal");
            token_clone.cancel();
        }
    });

    // wait for signal handler to complete (which happens after Ctrl-C is received)
    tokio::select! {
        _ = sig_handle => {
            info!(log, "Signal handler task completed");
        }
        _ = token.cancelled() => {
            info!(log, "Cancellation token was cancelled");
        }
    }

    // Ensure the cancellation token is active if the indexer exited first
    token.cancel();

    // Wait for the consumer loop to stop
    match indexer_handle.await {
        Ok(Ok(())) => {}
        Ok(Err(err)) => {
            error!(log, "Indexer loop exited with error"; "error" => %err);
            process::exit(1);
        }
        Err(err) => {
            error!(log, "Indexer loop task panicked"; "error" => %err);
            process::exit(1);
        }
    }
    if let Err(err) = sweep_handle.await {
        error!(log, "Sweep loop task failed"; "error" => %err);
        process::exit(1);
    }

    // wait for all indexer tasks to complete, but with a timeout to prevent hanging indefinitely
    match timeout(std::time::Duration::from_secs(30), async {
        let mut tasks = indx.tasks.lock().await;
        while let Some(result) = tasks.join_next().await {
            if let Err(err) = result {
                error!(log, "Indexer worker task failed"; "error" => %err);
            }
        }
    })
    .await
    {
        Ok(_) => info!(log, "All indexer tasks completed"),
        Err(_) => info!(
            log,
            "Timeout reached while waiting for indexer tasks to complete"
        ),
    }

    info!(log, "Indexer service shutting down");
    indx.close().await;

    Ok(())
}
