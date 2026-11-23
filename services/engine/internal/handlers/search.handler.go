package handlers

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Hassan-ach/boogle/services/engine/internal/service"
	"github.com/Hassan-ach/boogle/services/engine/internal/store"

	"github.com/Hassan-ach/boogle/services/engine/view/page/result"

	"github.com/labstack/echo/v5"
)

// Query limits. Every token costs up to four cgo calls to aspell and the query
// is attacker-controlled, so the input must be bounded.
const (
	maxQueryRunes  = 200
	maxQueryTokens = 10
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
	tokens := queryTokens(c.QueryParam("query"))
	filter := c.QueryParam("tab")
	if filter == "" {
		filter = "all"
	}

	// The placeholders never use suggestions, so the spell checker is not run
	// for them.
	switch filter {
	case "images":
		return handleImagesTab(c)
	case "graph":
		return handleGraphTab(c)
	}

	return h.handleAllTab(c, tokens)
}

func (h SearchingHandler) handleAllTab(c *echo.Context, tokens []string) error {
	ctx := c.Request().Context()

	currentPage := getPageNum(c)

	sugs := tokens
	if len(tokens) > 0 {
		sugs = h.Speller.GetSuggestions(strings.Join(tokens, " "))
	}

	totalResults, err := h.Store.GetTotalResults(ctx, sugs)
	if err != nil {
		return err
	}

	data, err := h.Store.GetData(ctx, sugs)
	if err != nil {
		return err
	}

	ranked, err := h.Ranker.Rank(data)
	if err != nil {
		return err
	}

	pageSize := h.Store.PageSize()
	if pageSize < 1 {
		pageSize = 20
	}

	// The ranker returns the candidate window in final order, so slicing here makes
	// page 1 the best page. Bound advertised pages by the results in hand.
	reachable := len(ranked)
	if totalResults < reachable {
		reachable = totalResults
	}
	totalPages := (reachable + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	currentPage = clampPage(currentPage, totalPages)

	start := (currentPage - 1) * pageSize
	if start > len(ranked) {
		start = len(ranked)
	}
	end := start + pageSize
	if end > len(ranked) {
		end = len(ranked)
	}

	// htmx sets HX-Request on its partial swaps; the view returns the inner
	// content of #results instead of a full page when it is set.
	isHtmx := c.Request().Header.Get("HX-Request") == "true"

	// Advertise the reachable count, not the unbounded DB count, so a large match
	// set with a small window does not overstate the available pages.
	return render(c, result.ShowAll(ranked[start:end], reachable, totalPages, currentPage, isHtmx))
}

func handleImagesTab(c *echo.Context) error {
	isHtmx := c.Request().Header.Get("HX-Request") == "true"
	return render(c, result.ShowImages(nil, isHtmx))
}

func handleGraphTab(c *echo.Context) error {
	isHtmx := c.Request().Header.Get("HX-Request") == "true"
	return render(c, result.ShowGraph(nil, isHtmx))
}

// queryTokens normalises a raw query into at most maxQueryTokens lower-case
// tokens of bounded total length. An empty result means "no searchable input".
func queryTokens(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	if utf8.RuneCountInString(raw) > maxQueryRunes {
		runes := []rune(raw)
		raw = string(runes[:maxQueryRunes])
	}

	tokens := strings.Fields(strings.ToLower(raw))
	if len(tokens) > maxQueryTokens {
		tokens = tokens[:maxQueryTokens]
	}
	return tokens
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

// clampPage keeps a stale ?page= from dead-ending: it renders the last real
// page instead of an empty result set with no way back.
func clampPage(page, totalPages int) int {
	if page < 1 {
		return 1
	}
	if page > totalPages {
		return totalPages
	}
	return page
}
