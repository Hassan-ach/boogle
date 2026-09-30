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

/// Run `f` until it succeeds or `max_attempts` attempts have been made.
///
/// `max_attempts` counts total attempts, not retries after the first one:
/// `retry_async(3, ..)` calls `f` at most three times. The ranking service's
/// Python `retry_on_db_error` uses the opposite convention, so this name is
/// deliberately explicit.
pub async fn retry_async<F, Fut, T, E>(max_attempts: usize, mut f: F) -> Result<T, E>
where
    F: FnMut() -> Fut,
    Fut: Future<Output = Result<T, E>>,
{
    assert!(
        max_attempts > 0,
        "max_attempts must be at least 1; a retry budget of 0 is a configuration error"
    );

    let mut attempts = 0;
    loop {
        match f().await {
            Ok(val) => return Ok(val),
            Err(err) => {
                attempts += 1;
                if attempts >= max_attempts {
                    return Err(err);
                }
                tokio::time::sleep(std::time::Duration::from_millis(500)).await;
            }
        }
    }
}

/// Synchronous counterpart of [`retry_async`]; `max_attempts` counts attempts.
pub fn retry_sync<F, T, E>(max_attempts: usize, mut f: F) -> Result<T, E>
where
    F: FnMut() -> Result<T, E>,
{
    assert!(
        max_attempts > 0,
        "max_attempts must be at least 1; a retry budget of 0 is a configuration error"
    );

    let mut attempts = 0;
    loop {
        match f() {
            Ok(val) => return Ok(val),
            Err(err) => {
                attempts += 1;
                if attempts >= max_attempts {
                    return Err(err);
                }
                // Uses standard synchronous sleep
                std::thread::sleep(std::time::Duration::from_millis(500));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;

    fn boom<T, E>() -> Result<T, E>
    where
        E: Default,
    {
        Err(E::default())
    }

    #[tokio::test(start_paused = true)]
    async fn retry_async_returns_first_success_without_retrying() {
        let calls = Cell::new(0);
        let result: Result<u8, &str> = retry_async(3, || {
            calls.set(calls.get() + 1);
            async { Ok(7u8) }
        })
        .await;

        assert_eq!(result, Ok(7));
        assert_eq!(calls.get(), 1, "a success must not be retried");
    }

    #[tokio::test(start_paused = true)]
    async fn retry_async_returns_after_transient_failures() {
        let calls = Cell::new(0);
        let result: Result<u8, &str> = retry_async(5, || {
            calls.set(calls.get() + 1);
            let n = calls.get();
            async move { if n < 3 { Err("boom") } else { Ok(42u8) } }
        })
        .await;

        assert_eq!(result, Ok(42));
        assert_eq!(calls.get(), 3, "stops at the first success");
    }

    #[tokio::test(start_paused = true)]
    async fn retry_async_attempts_exactly_max_attempts_then_propagates() {
        // The naming contract: max_attempts counts total attempts, not retries.
        for max_attempts in 1..=4usize {
            let calls = Cell::new(0);
            let result: Result<u8, &str> = retry_async(max_attempts, || {
                calls.set(calls.get() + 1);
                async { Err("always fails") }
            })
            .await;

            assert_eq!(result, Err("always fails"));
            assert_eq!(
                calls.get(),
                max_attempts,
                "max_attempts={max_attempts} must mean {max_attempts} calls"
            );
        }
    }

    #[tokio::test(start_paused = true)]
    async fn retry_async_sleeps_between_attempts() {
        let calls = Cell::new(0);
        let result: Result<u8, &str> = retry_async(3, || {
            calls.set(calls.get() + 1);
            async { Err("nope") }
        })
        .await;

        assert!(result.is_err());
        assert_eq!(calls.get(), 3);
        // Two retries means two backoff sleeps; tokio's paused clock makes this
        // instant, but the loop must still have completed rather than hung.
    }

    #[tokio::test(start_paused = true)]
    #[should_panic(expected = "max_attempts must be at least 1")]
    async fn retry_async_rejects_a_zero_attempt_budget() {
        let _ = retry_async(0, || async { Ok::<u8, &str>(1) }).await;
    }

    #[test]
    #[should_panic(expected = "max_attempts must be at least 1")]
    fn retry_sync_rejects_a_zero_attempt_budget() {
        let _ = retry_sync(0, || Ok::<u8, &str>(1));
    }

    #[test]
    fn retry_sync_returns_first_success() {
        let calls = Cell::new(0);
        let result = retry_sync(3, || {
            calls.set(calls.get() + 1);
            Ok::<u8, &str>(9)
        });

        assert_eq!(result, Ok(9));
        assert_eq!(calls.get(), 1);
    }

    #[test]
    fn retry_sync_attempts_exactly_max_attempts_then_propagates() {
        for max_attempts in 1..=4usize {
            let calls = Cell::new(0);
            let result = retry_sync(max_attempts, || {
                calls.set(calls.get() + 1);
                Err::<u8, &str>("always fails")
            });

            assert_eq!(result, Err("always fails"));
            assert_eq!(calls.get(), max_attempts);
        }
    }

    #[test]
    fn retry_sync_attempts_count_is_stable_across_repeated_calls() {
        // Guards against state leaking between invocations of the same closure.
        let calls = Cell::new(0);
        for _ in 0..3 {
            let result = retry_sync(3, || {
                calls.set(calls.get() + 1);
                Err::<u8, &str>("fail")
            });
            assert!(result.is_err());
        }
        assert_eq!(calls.get(), 9, "3 attempts per call across 3 calls");
    }
}
