package apperror

import "net/http"

// AppError carries an HTTP status alongside the cause. Message is what reaches
// the client; Err stays wrapped so errors.Is and errors.As keep working, and its
// text must never be shown to a user.
type AppError struct {
	Code    int
	Message string
	Err     error
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

// Internal is the 500 constructor: the cause is logged and only the fixed message
// is returned.
func Internal(err error) *AppError {
	return &AppError{
		Code:    http.StatusInternalServerError,
		Message: "internal server error",
		Err:     err,
	}
}
