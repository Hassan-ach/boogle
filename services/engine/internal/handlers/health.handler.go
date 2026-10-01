package handlers

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

type HealthHandler struct{}

func (h HealthHandler) Handle(c *echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}
