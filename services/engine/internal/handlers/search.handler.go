package handlers

import (
	"strconv"

	"github.com/Hassan-ach/boogle/services/engine/internal/service"
	"github.com/Hassan-ach/boogle/services/engine/internal/store"

	"github.com/Hassan-ach/boogle/services/engine/view/page/result"

	"github.com/labstack/echo/v5"
)

type SearchingHandler struct {
	Ranker  service.Ranker
	Speller service.Speller
	Store   store.Store
}

func NewSearchHandler(
	store store.Store,
	ranker service.Ranker,
	speller service.Speller,
) *SearchingHandler {
	return &SearchingHandler{
		ranker,
		speller,
		store,
	}
}

// Handle dispatches on the requested tab. The "images" and "graph" tabs render
// empty placeholders; only "all" queries the store and the ranker.
func (h SearchingHandler) Handle(c *echo.Context) error {
	query := c.QueryParam("query")
	filter := c.QueryParam("tab")
	if filter == "" {
		filter = "all"
	}

	sugs := h.Speller.GetSuggestions(query)

	switch filter {
	case "images":
		return handleImagesTab(c)
	case "graph":
		return handleGraphTab(c)
	}

	return h.handleAllTab(c, sugs)
}

func (h SearchingHandler) handleAllTab(c *echo.Context, sugs []string) error {
	ctx := c.Request().Context()

	currentPage := getPageNum(c)

	totalPages, err := h.Store.GetTotalPages(ctx, sugs)
	if err != nil {
		return err
	}

	data, err := h.Store.GetData(ctx, sugs, currentPage-1)
	if err != nil {
		return err
	}

	pages, err := h.Ranker.Rank(data)
	if err != nil {
		return err
	}

	// htmx sets HX-Request on its partial swaps; the view returns a fragment
	// instead of a full page when it is set.
	isHtmx := c.Request().Header.Get("HX-Request") == "true"

	return render(c, result.ShowAll(pages, totalPages, currentPage, isHtmx))
}

func handleImagesTab(c *echo.Context) error {
	isHtmx := c.Request().Header.Get("HX-Request") == "true"
	return render(c, result.ShowImages(nil, isHtmx))
}

func handleGraphTab(c *echo.Context) error {
	isHtmx := c.Request().Header.Get("HX-Request") == "true"
	return render(c, result.ShowGraph(nil, isHtmx))
}

// getPageNum returns a 1-based page number, defaulting to 1. A missing,
// unparseable or non-positive value falls back rather than erroring, so a
// hand-edited or stale pagination link still returns results.
func getPageNum(c *echo.Context) int {
	page := c.QueryParam("page")
	pageNum := 1

	if page != "" {
		v, err := strconv.Atoi(page)
		if err == nil && v > 0 {
			pageNum = v
		}
	}
	return pageNum
}
