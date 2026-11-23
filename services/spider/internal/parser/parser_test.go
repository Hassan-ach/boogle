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

func TestA_FetchedPagesIdentityIsTheFetchedURLNotOgURL(t *testing.T) {
	html := `<!doctype html><html lang="en"><head>` +
		`<meta property="og:url" content="https://victim.example/trusted">` +
		`<title>attacker</title></head><body>hello</body></html>`

	p := NewParser(nil, newTestLogger())
	page, err := p.ParseHTML(strings.NewReader(html), "https://evil.example/page")
	if err != nil {
		t.Fatalf("ParseHTML failed: %v", err)
	}

	if page.URL != "https://evil.example/page" {
		t.Fatalf("identity followed og:url to %q", page.URL)
	}
	if page.MetaData.OgURL != "" {
		t.Errorf("a cross-host og:url was kept as metadata: %q", page.MetaData.OgURL)
	}
}

func TestOgURLOnTheSameHostIsKeptAsDisplayMetadata(t *testing.T) {
	html := `<!doctype html><html lang="en"><head>` +
		`<meta property="og:url" content="https://example.com/canonical">` +
		`<title>t</title></head><body>hello</body></html>`

	p := NewParser(nil, newTestLogger())
	page, err := p.ParseHTML(strings.NewReader(html), "https://example.com/page")
	if err != nil {
		t.Fatalf("ParseHTML failed: %v", err)
	}

	if page.URL != "https://example.com/page" {
		t.Fatalf("identity changed to %q", page.URL)
	}
	if page.MetaData.OgURL != "https://example.com/canonical" {
		t.Errorf("same-host og:url was dropped: %q", page.MetaData.OgURL)
	}
}
