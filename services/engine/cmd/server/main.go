package main

import (
	"embed"
	"log"
	"time"

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
	conf, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	store := store.NewStore(conf.Store)

	var speller service.Speller
	aspellSpeller, err := spellchecker.NewAspellSpellingService()
	if err != nil {
		speller = spellchecker.NewDummySpellingService()
	} else {
		speller = aspellSpeller
	}

	ranker := ranking.NewRankingService(conf.Ranker)

	homeHandler := &handlers.HomeHandler{}
	rankingHandler := handlers.NewSearchHandler(store, ranker, speller)
	healthHandler := &handlers.HealthHandler{}

	e := echo.New()

	e.HTTPErrorHandler = handlers.HandleError
	e.Use(echoMiddleware.Recover())
	e.Use(echoMiddleware.RequestID())
	// Log path only: the stock RequestLogger logs the full URI, including search terms.
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			started := time.Now()
			err := next(c)
			status := 0
			if response, ok := c.Response().(*echo.Response); ok {
				status = response.Status
			}
			e.Logger.Info("request",
				"method", c.Request().Method,
				"path", c.Request().URL.Path,
				"status", status,
				"latency", time.Since(started).String(),
			)
			return err
		}
	})
	e.Use(echoMiddleware.GzipWithConfig(echoMiddleware.GzipConfig{Level: 5}))
	e.Use(echoMiddleware.RateLimiter(echoMiddleware.NewRateLimiterMemoryStore(20)))

	e.Static("/public", "public")
	e.GET("/", homeHandler.Handle)
	e.GET("/search", rankingHandler.Handle)
	e.GET("/health", healthHandler.Handle)
	e.GET("/feeling-lucky", func(c *echo.Context) error {
		return c.String(200, "GOOD FOR YOU")
	})

	if err := e.Start(":1323"); err != nil {
		e.Logger.Error("failed to start server", "error", err)
	}
}
