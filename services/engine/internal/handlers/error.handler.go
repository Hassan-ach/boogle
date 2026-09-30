package handlers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Hassan-ach/boogle/services/engine/internal/apperror"
	errorpage "github.com/Hassan-ach/boogle/services/engine/view/page/error"

	"github.com/labstack/echo/v5"
)

func HandleError(c *echo.Context, err error) {
	code, message, internalErr := classifyError(err)

	if internalErr != nil {
		c.Logger().
			Error(fmt.Sprintf("[%s] %s → %v", c.Request().Method, c.Request().URL.Path, internalErr.Error()))
	}

	c.Response().WriteHeader(code)
	err = render(c, errorpage.ErrorPage(code, message))

	if err != nil {
		c.Logger().Error("Failed to render error page", "error", err)
	}
}

func classifyError(err error) (int, string, error) {
	if errors.Is(err, echo.ErrNotFound) {
		return http.StatusNotFound, "page not found", nil
	}

	if errors.Is(err, echo.ErrMethodNotAllowed) {
		return http.StatusMethodNotAllowed, "method not allowed", nil
	}

	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		// A hand-built AppError with no Code would otherwise reach
		// WriteHeader(0), and net/http panics on a status code outside 100..599.
		// Taking that as "unset" keeps the error path from turning a store
		// failure into a crashed request handler.
		code := appErr.Code
		if code < 100 || code > 599 {
			code = http.StatusInternalServerError
		}
		return code, appErr.Message, appErr.Err
	}

	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		message := httpErr.Message
		if message == "" {
			message = http.StatusText(httpErr.Code)
		}
		return httpErr.Code, message, nil
	}

	return http.StatusInternalServerError, "internal server error", err
}
