use std::io;

use thiserror::Error;

#[derive(Error, Debug)]
pub enum DatabaseError {
    #[error("Database error: {0}")]
    SqlxError(#[from] sqlx::Error),
    #[error("Database connection error: {0}")]
    ConnectionError(String),
    #[error("Database query error: {0}")]
    QueryError(String),
    #[error("Database not found error: {0}")]
    NotFoundError(String),
    #[error("Database batch words error: {0}")]
    BatchWordsError(String),
}

#[derive(Error, Debug)]
pub enum MessagingError {
    #[error("Messaging error: {0}")]
    LapinError(lapin::Error),
    #[error("Messaging connection error: {0}")]
    ConnectionError(String),
    #[error("Messaging channel error: {0}")]
    ChannelError(String),
    #[error("Messaging consume error: {0}")]
    ConsumeError(String),
    #[error("Messaging publish error: {0}")]
    PublishError(String),
    #[error("Messaging declare queue error: {0}")]
    DeclareQueueError(String),
}

#[derive(Error, Debug)]
pub enum IndexerError {
    #[error(transparent)]
    Database(#[from] DatabaseError),
    #[error(transparent)]
    Messaging(#[from] MessagingError),
    #[error("Serde error: {0}")]
    SerdeError(serde_json::Error),
    #[error("Other error: {0}")]
    Other(String),
    #[error("Parse error: {0}")]
    ParseError(String),
}

pub type AppError = IndexerError;

impl From<sqlx::Error> for IndexerError {
    fn from(err: sqlx::Error) -> Self {
        let db_err = DatabaseError::SqlxError(err);
        IndexerError::Database(db_err)
    }
}

impl From<lapin::Error> for IndexerError {
    fn from(err: lapin::Error) -> Self {
        IndexerError::Messaging(MessagingError::LapinError(err))
    }
}

impl From<serde_json::Error> for IndexerError {
    fn from(err: serde_json::Error) -> Self {
        IndexerError::SerdeError(err)
    }
}

impl From<String> for IndexerError {
    fn from(err: String) -> Self {
        IndexerError::Other(err)
    }
}

impl From<io::Error> for IndexerError {
    fn from(err: io::Error) -> Self {
        IndexerError::Other(err.to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::core::messaging::Job;

    #[test]
    fn sqlx_errors_become_database_errors() {
        let err: AppError = AppError::from(sqlx::Error::PoolClosed);
        match &err {
            AppError::Database(DatabaseError::SqlxError(_)) => {}
            other => panic!("expected a wrapped sqlx error, got {other:?}"),
        }
        assert!(err.to_string().starts_with("Database error:"));
    }

    #[test]
    fn serde_errors_become_serde_errors() {
        let json_err = serde_json::from_slice::<Job>(b"{not json").unwrap_err();
        let err: AppError = json_err.into();

        assert!(matches!(err, AppError::SerdeError(_)));
        assert!(err.to_string().contains("Serde error"));
    }

    #[test]
    fn strings_become_other_errors() {
        let err: AppError = "something broke".to_string().into();
        assert!(matches!(err, AppError::Other(ref m) if m == "something broke"));
        assert_eq!(err.to_string(), "Other error: something broke");
    }

    #[test]
    fn io_errors_become_other_errors_carrying_the_message() {
        let io_err = io::Error::new(io::ErrorKind::UnexpectedEof, "eof reached");
        let err: AppError = io_err.into();

        match &err {
            AppError::Other(msg) => assert!(msg.contains("eof reached")),
            other => panic!("expected Other, got {other:?}"),
        }
    }

    #[test]
    fn lapin_errors_become_messaging_errors() {
        let io_err = io::Error::new(io::ErrorKind::BrokenPipe, "broker hung up");
        let lapin_err: lapin::Error = io_err.into();
        let err: AppError = AppError::from(lapin_err);

        match &err {
            AppError::Messaging(MessagingError::LapinError(inner)) => {
                assert!(inner.to_string().contains("broker hung up"));
            }
            other => panic!("expected a lapin error, got {other:?}"),
        }
    }

    #[test]
    fn error_messages_name_their_source() {
        let cases: Vec<AppError> = vec![
            DatabaseError::ConnectionError("db down".into()).into(),
            DatabaseError::QueryError("bad sql".into()).into(),
            DatabaseError::NotFoundError("page 7 missing".into()).into(),
            DatabaseError::BatchWordsError("upsert failed".into()).into(),
            MessagingError::ConnectionError("broker down".into()).into(),
            MessagingError::ChannelError("no channel".into()).into(),
            MessagingError::ConsumeError("consume failed".into()).into(),
            MessagingError::PublishError("publish failed".into()).into(),
            MessagingError::DeclareQueueError("declare failed".into()).into(),
        ];

        for err in cases {
            let rendered = err.to_string();
            assert!(!rendered.is_empty(), "{err:?} rendered as an empty string");
            assert!(
                !rendered.contains('{') && !rendered.contains('}'),
                "{err:?} rendered with an unsubstituted placeholder: {rendered}"
            );
        }
    }

    #[test]
    fn database_and_messaging_wrappers_are_transparent() {
        let inner = DatabaseError::NotFoundError("page 42".into());
        let expected = inner.to_string();
        let wrapped: IndexerError = inner.into();
        assert_eq!(wrapped.to_string(), expected);

        let source = std::error::Error::source(&wrapped);
        assert!(
            source.is_none(),
            "transparent wrappers expose no extra layer"
        );
    }
}
