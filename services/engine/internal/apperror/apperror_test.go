package apperror

import (
	"errors"
	"net/http"
	"testing"
)

func TestInternalReturnsAnAppError(t *testing.T) {
	base := errors.New("database is down")
	appErr := Internal(base)

	if appErr == nil {
		t.Fatal("Internal returned nil")
	}
	if appErr.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want %d", appErr.Code, http.StatusInternalServerError)
	}
	if appErr.Message != "internal server error" {
		t.Errorf("Message = %q, want %q", appErr.Message, "internal server error")
	}
	if !errors.Is(appErr, base) {
		t.Error("AppError does not wrap the base error")
	}
	if errors.Unwrap(appErr) != base {
		t.Errorf("Unwrap() = %v, want %v", errors.Unwrap(appErr), base)
	}
	if appErr.Error() != "internal server error" {
		t.Errorf("Error() = %q, want %q", appErr.Error(), "internal server error")
	}
}
