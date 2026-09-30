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
    pub lock_timeout_ms: u64,
    pub statement_timeout_ms: u64,
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
    let lock_timeout_ms = env::var("PG_LOCK_TIMEOUT_MS")
        .unwrap_or_else(|_| "5000".to_string())
        .parse::<u64>()
        .expect("PG_LOCK_TIMEOUT_MS must be a number");
    let statement_timeout_ms = env::var("PG_STATEMENT_TIMEOUT_MS")
        .unwrap_or_else(|_| "60000".to_string())
        .parse::<u64>()
        .expect("PG_STATEMENT_TIMEOUT_MS must be a number");
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
        lock_timeout_ms,
        statement_timeout_ms,
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Mutex, MutexGuard, OnceLock};

    // Environment variables are process-global, so the tests that mutate them
    // must not run concurrently.
    static ENV_LOCK: OnceLock<Mutex<()>> = OnceLock::new();

    fn env_lock() -> MutexGuard<'static, ()> {
        ENV_LOCK
            .get_or_init(|| Mutex::new(()))
            .lock()
            .unwrap_or_else(|e| e.into_inner())
    }

    /// Rust 2024 marks `set_var`/`remove_var` unsafe because they mutate
    /// process-global state. Every caller holds `env_lock`, so no other thread
    /// can be reading the environment concurrently.
    fn set_var(key: &str, value: &str) {
        unsafe { env::set_var(key, value) }
    }

    fn remove_var(key: &str) {
        unsafe { env::remove_var(key) }
    }

    /// Clear every variable these loaders read so each test starts from a known
    /// state regardless of what the developer has exported.
    fn clear_env() {
        for key in [
            "LOG_PATH",
            "RABBITMQ_QUEUE",
            "RABBITMQ_CONFIRMATION_QUEUE",
            "PG_MAX_CONNECTIONS",
            "PG_MIN_CONNECTIONS",
            "ACQUIRE_TIMEOUT_SECONDS",
            "PG_LOCK_TIMEOUT_MS",
            "PG_STATEMENT_TIMEOUT_MS",
            "WORD_BATCH_SIZE",
            "PAGE_WORD_BATCH_SIZE",
            "MAX_RETRIES",
            "SWEEP_INTERVAL_SECONDS",
            "SWEEP_GRACE_SECONDS",
        ] {
            remove_var(key);
        }
        // RABBITMQ_URL and DATABASE_URL are required; provide them by default.
        set_var("DATABASE_URL", "postgres://user:pass@localhost:5432/db");
        set_var("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/%2f");
    }

    #[test]
    fn load_app_config_uses_defaults_when_unset() {
        let _guard = env_lock();
        clear_env();

        let app = load_app_config();

        assert_eq!(app.log_path, "indexer.log");
        assert_eq!(app.queue_name, "indexer.jobs");
        assert_eq!(app.confirmation_queue_name, "indexer.confirmations");
        assert_eq!(app.max_concurrent_tasks, 10);
        assert_eq!(app.sweep_interval, Duration::from_secs(300));
        assert_eq!(app.sweep_grace, Duration::from_secs(600));
    }

    #[test]
    fn load_app_config_reads_overrides() {
        let _guard = env_lock();
        clear_env();

        set_var("LOG_PATH", "/tmp/custom.log");
        set_var("RABBITMQ_QUEUE", "custom.jobs");
        set_var("RABBITMQ_CONFIRMATION_QUEUE", "custom.confirmations");
        set_var("PG_MAX_CONNECTIONS", "42");
        set_var("SWEEP_INTERVAL_SECONDS", "30");
        set_var("SWEEP_GRACE_SECONDS", "90");

        let app = load_app_config();

        assert_eq!(app.log_path, "/tmp/custom.log");
        assert_eq!(app.queue_name, "custom.jobs");
        assert_eq!(app.confirmation_queue_name, "custom.confirmations");
        assert_eq!(app.max_concurrent_tasks, 42);
        assert_eq!(app.sweep_interval, Duration::from_secs(30));
        assert_eq!(app.sweep_grace, Duration::from_secs(90));
    }

    #[test]
    fn unparseable_sweep_interval_falls_back_to_the_default() {
        let _guard = env_lock();
        clear_env();

        // These use `.unwrap_or(...)` rather than `.expect(...)`, so bad input
        // must degrade to the default instead of aborting startup.
        set_var("SWEEP_INTERVAL_SECONDS", "not-a-number");
        set_var("SWEEP_GRACE_SECONDS", "");

        let app = load_app_config();

        assert_eq!(app.sweep_interval, Duration::from_secs(300));
        assert_eq!(app.sweep_grace, Duration::from_secs(600));
    }

    #[test]
    #[should_panic(expected = "PG_MAX_CONNECTIONS must be a number")]
    fn non_numeric_pool_size_aborts_startup() {
        let _guard = env_lock();
        clear_env();
        set_var("PG_MAX_CONNECTIONS", "many");
        load_app_config();
    }

    #[test]
    fn load_psql_config_uses_defaults_when_unset() {
        let _guard = env_lock();
        clear_env();

        let psql = load_psql_config();

        assert_eq!(psql.url, "postgres://user:pass@localhost:5432/db");
        assert_eq!(psql.max_connections, 10);
        assert_eq!(psql.min_connections, 2);
        assert_eq!(psql.acquire_timeout_seconds, Duration::from_secs(5));
        assert_eq!(psql.lock_timeout_ms, 5000);
        assert_eq!(psql.statement_timeout_ms, 60000);
        assert_eq!(psql.word_batch_size, 1000);
        assert_eq!(psql.page_word_batch_size, 500);
        assert_eq!(psql.max_retries, 3);
    }

    #[test]
    fn load_psql_config_reads_overrides() {
        let _guard = env_lock();
        clear_env();

        set_var("PG_MIN_CONNECTIONS", "5");
        set_var("ACQUIRE_TIMEOUT_SECONDS", "17");
        set_var("PG_LOCK_TIMEOUT_MS", "1234");
        set_var("PG_STATEMENT_TIMEOUT_MS", "9999");
        set_var("WORD_BATCH_SIZE", "7");
        set_var("PAGE_WORD_BATCH_SIZE", "11");
        set_var("MAX_RETRIES", "9");

        let psql = load_psql_config();

        assert_eq!(psql.min_connections, 5);
        assert_eq!(psql.acquire_timeout_seconds, Duration::from_secs(17));
        assert_eq!(psql.lock_timeout_ms, 1234);
        assert_eq!(psql.statement_timeout_ms, 9999);
        assert_eq!(psql.word_batch_size, 7);
        assert_eq!(psql.page_word_batch_size, 11);
        assert_eq!(psql.max_retries, 9);
    }

    #[test]
    #[should_panic(expected = "DATABASE_URL must be set")]
    fn missing_database_url_aborts_startup() {
        let _guard = env_lock();
        clear_env();
        remove_var("DATABASE_URL");
        load_psql_config();
    }

    #[test]
    fn load_rabbit_config_reads_values() {
        let _guard = env_lock();
        clear_env();

        let rabbit = load_rabbit_config();

        assert_eq!(rabbit.url, "amqp://guest:guest@localhost:5672/%2f");
        assert_eq!(rabbit.queue, "indexer.jobs");
        assert_eq!(rabbit.confirmation_queue_name, "indexer.confirmations");
    }

    #[test]
    #[should_panic(expected = "RABBITMQ_URL must be set")]
    fn missing_rabbit_url_aborts_startup() {
        let _guard = env_lock();
        clear_env();
        remove_var("RABBITMQ_URL");
        load_rabbit_config();
    }

    #[test]
    fn queue_names_agree_between_app_and_rabbit_config() {
        // The indexer consumes `app.queue_name` and the ranking service listens on
        // `confirmation_queue_name`; a mismatch silently stops all ranking.
        let _guard = env_lock();
        clear_env();
        set_var("RABBITMQ_QUEUE", "a.jobs");
        set_var("RABBITMQ_CONFIRMATION_QUEUE", "a.confirmations");

        let app = load_app_config();
        let rabbit = load_rabbit_config();

        assert_eq!(app.queue_name, rabbit.queue);
        assert_eq!(app.confirmation_queue_name, rabbit.confirmation_queue_name);
    }
}
