use dotenv::from_path;
use std::env;

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
}

#[derive(Debug, Clone)]
pub struct AppConfig {
    pub log_path: String,
    pub loop_delay_ms: u64,
    pub indexer_count: usize,
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
    let loop_delay_ms = env::var("LOOP_DELAY_MS")
        .unwrap_or_else(|_| "100".to_string())
        .parse::<u64>()
        .expect("LOOP_DELAY_MS must be a number");
    let indexer_count = env::var("INDEXER_COUNT")
        .unwrap_or_else(|_| "4".to_string())
        .parse::<usize>()
        .expect("INDEXER_COUNT must be a number");

    AppConfig {
        log_path,
        loop_delay_ms,
        indexer_count,
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

    PsqlConfig {
        url,
        max_connections,
        min_connections,
        acquire_timeout_seconds: std::time::Duration::from_secs(acquire_timeout),
        word_batch_size,
        page_word_batch_size,
    }
}

fn load_rabbit_config() -> RabbitConfig {
    let url = env::var("RABBITMQ_URL").expect("RABBITMQ_URL must be set");
    let queue = env::var("RABBITMQ_QUEUE").unwrap_or_else(|_| "indexer.jobs".to_string());
    RabbitConfig { url, queue }
}
