package parser

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

func newTestLogger() *utils.Logger {
	return &utils.Logger{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestParseRobots(t *testing.T) {
	robotsTxt := `
User-agent: *
Disallow: /private/
Disallow: /admin/
Crawl-delay: 10
Sitemap: https://example.com/sitemap.xml
`

	p := NewParser(nil, newTestLogger())
	r := p.ParseRobots(robotsTxt, "*")

	if r.CrawlDelay != 10 {
		t.Errorf("expected CrawlDelay 10, got %d", r.CrawlDelay)
	}

	if len(r.Disallow) != 2 {
		t.Errorf("expected 2 Disallow rules, got %d", len(r.Disallow))
	}

	if len(r.SiteMaps) != 1 || r.SiteMaps[0] != "https://example.com/sitemap.xml" {
		t.Errorf("expected Sitemap https://example.com/sitemap.xml, got %v", r.SiteMaps)
	}
}

func TestParseHTML(t *testing.T) {
	htmlContent := `
<!DOCTYPE html>
<html lang="en">
<head>
    <title>Test Page Title</title>
    <meta name="description" content="A test page description.">
</head>
<body>
    <h1>Welcome to Test Page</h1>
    <p>This is a paragraph with <a href="https://example.com/about">About Us</a> link.</p>
</body>
</html>
`

	p := NewParser(nil, newTestLogger())
	page, err := p.ParseHTML(strings.NewReader(htmlContent), "https://example.com")
	if err != nil {
		t.Fatalf("ParseHTML failed: %v", err)
	}

	if page.MetaData.Title != "Test Page Title" {
		t.Errorf("expected Title 'Test Page Title', got '%s'", page.MetaData.Title)
	}

	if page.MetaData.Description != "A test page description." {
		t.Errorf("expected Description 'A test page description.', got '%s'", page.MetaData.Description)
	}

	if len(page.Links) == 0 {
		t.Errorf("expected links to be parsed, got 0")
	}
}

// TestAPageThatListsTheSameURLTwiceCrawlsItOnce covers the dedup, which is easy to
// lose and expensive to lose.
//
// Almost every page links to its own header and footer. The three spellings below
// are one document -- an absolute URL, a root-relative one and the same root-
// relative one with a fragment -- and they reach the collector as three separate
// hrefs. Canonicalisation collapses all three to the same key, which is the point
// of canonicalising first: two URLs that differ only in spelling are one page, and
// keying them separately splits its word counts and its PageRank between them.
//
// The frontier is a set, so a duplicate is harmless there. It is not harmless
// before it: every duplicate costs an admission, a state read and a counter
// increment, on every page that has a nav bar, and the refusals among them are
// counted twice -- so a site's stats overstate how much of it was declined.
func TestAPageThatListsTheSameURLTwiceCrawlsItOnce(t *testing.T) {
	html := `<!doctype html><html lang="en"><head><title>t</title></head><body>` +
		`<a href="/about">about</a>` +
		`<a href="https://example.com/about">about again</a>` +
		`<a href="/about#team">about a third time</a>` +
		`<a href="/other">other</a>` +
		`</body></html>`

	p := NewParser(nil, newTestLogger())
	page, err := p.ParseHTML(strings.NewReader(html), "https://example.com/")
	if err != nil {
		t.Fatalf("ParseHTML failed: %v", err)
	}

	want := []string{"https://example.com/about", "https://example.com/other"}
	if len(page.Links) != len(want) {
		t.Fatalf("extracted %d links, want %d: %v", len(page.Links), len(want), page.Links)
	}
	for i := range want {
		if page.Links[i] != want[i] {
			t.Errorf("Links[%d] = %q, want %q", i, page.Links[i], want[i])
		}
	}
}
