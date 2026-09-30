package apperror

import "net/http"

type AppError struct {
	Code    int    // HTTP status code
	Message string // user-facing message
	Err     error  // internal cause (never sent to client)
}

func (e *AppError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *AppError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Constructors

func Internal(err error) *AppError {
	return &AppError{
		Code:    http.StatusInternalServerError,
		Message: "internal server error",
		Err:     err,
	}
}
