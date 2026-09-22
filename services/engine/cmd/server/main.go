package main

import (
	"embed"

	"github.com/Hassan-ach/boogle/services/engine/internal/config"
	"github.com/Hassan-ach/boogle/services/engine/internal/handlers"
	"github.com/Hassan-ach/boogle/services/engine/internal/service"
	"github.com/Hassan-ach/boogle/services/engine/internal/service/ranking"
	"github.com/Hassan-ach/boogle/services/engine/internal/service/spellchecker"
	"github.com/Hassan-ach/boogle/services/engine/internal/store"

	"github.com/labstack/echo/v5"
	echoMiddleware "github.com/labstack/echo/v5/middleware"
)

var embeddedFiles embed.FS

func main() {
	conf, _ := config.LoadConfig()

	store := store.NewStore(conf.Store)

	var speller service.Speller
	speller, err := spellchecker.NewAspellSpellingService()
	if err != nil {
		speller = spellchecker.NewDummySpellingService()
	}

	ranker := ranking.NewRankingService(conf.Ranker)

	homeHandler := &handlers.HomeHandler{}
	rankingHandler := handlers.NewSearchHandler(store, ranker, speller)

	e := echo.New()

	e.HTTPErrorHandler = handlers.HandleError
	e.Use(echoMiddleware.Recover())
	e.Use(echoMiddleware.RequestID())
	e.Use(echoMiddleware.RequestLogger())
	// e.Use(echoMiddleware.CORS())
	// e.Use(echoMiddleware.Secure())
	e.Use(echoMiddleware.GzipWithConfig(echoMiddleware.GzipConfig{Level: 5}))
	e.Use(echoMiddleware.RateLimiter(echoMiddleware.NewRateLimiterMemoryStore(20)))

	e.Static("/public", "public")
	e.GET("/", homeHandler.Handle)
	e.GET("/search", rankingHandler.Handle)
	e.GET("/feeling-lucky", func(c *echo.Context) error {
		return c.String(200, "GOOD FOR YOU")
	})

	if err := e.Start(":1323"); err != nil {
		e.Logger.Error("failed to start server", "error", err)
	}
}
