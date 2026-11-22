package handlers

import (
	"github.com/Hassan-ach/boogle/services/engine/view/page/home"
	"github.com/labstack/echo/v5"
)

type HomeHandler struct{}

func (h HomeHandler) Handle(c *echo.Context) error {
	return render(c, home.Show())
}
