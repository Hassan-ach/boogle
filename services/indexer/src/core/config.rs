use dotenv::from_path;
use std::{env, time::Duration};

#[derive(Debug, Clone)]
pub struct Config {
    pub app: AppConfig,
    pub psql: PsqlConfig,
    pub rabbit: RabbitConfig,
}

#[derive(Debug, Clone)]
pub struct PsqlConfig {
    pub url: String,
    pub max_connections: u32,
    pub min_connections: u32,
    pub acquire_timeout_seconds: std::time::Duration,
    pub word_batch_size: usize,
    pub page_word_batch_size: usize,
    pub max_retries: usize,
}

#[derive(Debug, Clone)]
pub struct RabbitConfig {
    pub url: String,
    pub queue: String,
    pub confirmation_queue_name: String,
}

#[derive(Debug, Clone)]
pub struct AppConfig {
    pub log_path: String,
    pub queue_name: String,
    pub max_concurrent_tasks: usize,
    pub sweep_interval: std::time::Duration,
    pub sweep_grace: std::time::Duration,
    pub confirmation_queue_name: String,
}

pub fn load_config(env_path: String) -> Config {
    if let Err(e) = from_path(&env_path) {
        eprintln!("Could not load .env file: {}", e);
    }

    let app = load_app_config();
    let psql = load_psql_config();
    let rabbit = load_rabbit_config();
    Config { app, psql, rabbit }
}

fn load_app_config() -> AppConfig {
    let log_path = env::var("LOG_PATH").unwrap_or_else(|_| "indexer.log".to_string());
    let queue_name = env::var("RABBITMQ_QUEUE").unwrap_or_else(|_| "indexer.jobs".to_string());
    let max_concurrent_tasks = env::var("PG_MAX_CONNECTIONS")
        .unwrap_or_else(|_| "10".to_string())
        .parse::<usize>()
        .expect("PG_MAX_CONNECTIONS must be a number");
    let sweep_interval = Duration::from_secs(
        env::var("SWEEP_INTERVAL_SECONDS")
            .unwrap_or_else(|_| "300".into())
            .parse()
            .unwrap_or(300),
    );
    let sweep_grace = Duration::from_secs(
        env::var("SWEEP_GRACE_SECONDS")
            .unwrap_or_else(|_| "600".into())
            .parse()
            .unwrap_or(600),
    );
    let confirmation_queue_name = env::var("RABBITMQ_CONFIRMATION_QUEUE")
        .unwrap_or_else(|_| "indexer.confirmations".to_string());

    AppConfig {
        log_path,
        queue_name,
        max_concurrent_tasks,
        sweep_grace,
        sweep_interval,
        confirmation_queue_name,
    }
}

fn load_psql_config() -> PsqlConfig {
    let url = env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let max_connections = env::var("PG_MAX_CONNECTIONS")
        .unwrap_or_else(|_| "10".to_string())
        .parse::<u32>()
        .expect("PG_MAX_CONNECTIONS must be a number");
    let min_connections = env::var("PG_MIN_CONNECTIONS")
        .unwrap_or_else(|_| "2".to_string())
        .parse::<u32>()
        .expect("PG_MIN_CONNECTIONS must be a number");
    let acquire_timeout = env::var("ACQUIRE_TIMEOUT_SECONDS")
        .unwrap_or_else(|_| "5".to_string())
        .parse::<u64>()
        .expect("ACQUIRE_TIMEOUT_SECONDS must be a number");
    let word_batch_size = env::var("WORD_BATCH_SIZE")
        .unwrap_or_else(|_| "1000".to_string())
        .parse::<usize>()
        .expect("WORD_BATCH_SIZE must be a number");
    let page_word_batch_size = env::var("PAGE_WORD_BATCH_SIZE")
        .unwrap_or_else(|_| "500".to_string())
        .parse::<usize>()
        .expect("PAGE_WORD_BATCH_SIZE must be a number");
    let max_retries = env::var("MAX_RETRIES")
        .unwrap_or_else(|_| "3".to_string())
        .parse::<usize>()
        .expect("MAX_RETRIES must be a number");

    PsqlConfig {
        url,
        max_connections,
        min_connections,
        acquire_timeout_seconds: std::time::Duration::from_secs(acquire_timeout),
        word_batch_size,
        page_word_batch_size,
        max_retries,
    }
}

fn load_rabbit_config() -> RabbitConfig {
    let url = env::var("RABBITMQ_URL").expect("RABBITMQ_URL must be set");
    let queue = env::var("RABBITMQ_QUEUE").unwrap_or_else(|_| "indexer.jobs".to_string());
    let confirmation_queue_name = env::var("RABBITMQ_CONFIRMATION_QUEUE")
        .unwrap_or_else(|_| "indexer.confirmations".to_string());
    RabbitConfig {
        url,
        queue,
        confirmation_queue_name,
    }
}
