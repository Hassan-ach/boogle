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
