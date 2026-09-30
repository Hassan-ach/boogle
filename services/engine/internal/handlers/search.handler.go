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

	// The error is returned rather than written to the response. Writing it here
	// looked harmless and was not:
	//
	//     return c.String(http.StatusInternalServerError, fmt.Sprint("err: %w", err))
	//
	// `%w` is only meaningful to fmt.Errorf, so Sprint printed the verb
	// literally -- the body read "err: %wdial tcp 127.0.0.1:5432: connect:
	// connection refused" -- and the whole driver error, host and port included,
	// went to the browser. Returning the error hands it to HandleError, which
	// logs the cause and renders only the user-facing message, which is the whole
	// reason AppError keeps `Err` separate from `Message`. The store already
	// returns an *apperror.AppError, so there is nothing left to wrap.
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
