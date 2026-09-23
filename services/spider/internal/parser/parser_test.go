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
