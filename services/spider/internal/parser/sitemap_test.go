package parser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseSitemapReadsLocElements(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/a</loc><lastmod>2024-01-01</lastmod></url>
  <url><loc>https://example.com/b</loc></url>
</urlset>`

	got, err := parseSitemap([]byte(doc))
	if err != nil {
		t.Fatalf("parseSitemap() error: %v", err)
	}
	if len(got.Urls) != 2 {
		t.Fatalf("got %d urls, want 2", len(got.Urls))
	}
	if got.Urls[0].Loc != "https://example.com/a" {
		t.Errorf("Urls[0].Loc = %q", got.Urls[0].Loc)
	}
	if got.Urls[1].Loc != "https://example.com/b" {
		t.Errorf("Urls[1].Loc = %q", got.Urls[1].Loc)
	}
}

func TestParseSitemapAcceptsANamespacedDocument(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"
        xmlns:xhtml="http://www.w3.org/1999/xhtml">
  <url>
    <loc>https://example.com/a</loc>
    <xhtml:link rel="alternate" hreflang="en" href="https://example.com/en/a"/>
  </url>
</urlset>`

	got, err := parseSitemap([]byte(doc))
	if err != nil {
		t.Fatalf("parseSitemap() error: %v", err)
	}
	if len(got.Urls) != 1 || got.Urls[0].Loc != "https://example.com/a" {
		t.Errorf("parseSitemap() = %+v, want one URL", got)
	}
}

func TestParseSitemapOnAnEmptySitemap(t *testing.T) {
	got, err := parseSitemap([]byte(`<urlset></urlset>`))
	if err != nil {
		t.Fatalf("parseSitemap() error: %v", err)
	}
	if len(got.Urls) != 0 {
		t.Errorf("got %d urls, want 0", len(got.Urls))
	}
}

func TestParseSitemapRejectsMalformedXML(t *testing.T) {
	for name, doc := range map[string]string{
		"unclosed tag":    `<urlset><url><loc>https://example.com/a</urlset>`,
		"not xml at all":  `{"this": "is json"}`,
		"empty input":     ``,
		"mismatched tags": `<urlset></sitemapindex>`,
		"bare ampersand":  `<urlset><url><loc>https://example.com/?a=1&b=2</loc></url></urlset>`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSitemap([]byte(doc)); err == nil {
				t.Error("expected an error for malformed input")
			}
		})
	}
}

func TestParseSitemapOnAnHTMLErrorPage(t *testing.T) {
	got, err := parseSitemap([]byte(`<html><body>404 Not Found</body></html>`))
	if err != nil {
		t.Fatalf("parseSitemap() error: %v", err)
	}
	if len(got.Urls) != 0 {
		t.Errorf("got %d urls from an HTML page, want 0", len(got.Urls))
	}
}

func TestParseSitemapDoesNotFollowExternalEntities(t *testing.T) {
	doc := `<?xml version="1.0"?>
<!DOCTYPE lolz [
 <!ENTITY lol "lol">
 <!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">
]>
<urlset><url><loc>&lol2;</loc></url></urlset>`

	got, err := parseSitemap([]byte(doc))
	if err != nil {
		return
	}
	for _, entry := range got.Urls {
		if strings.Contains(entry.Loc, "lollollol") {
			t.Fatalf("entity was expanded: %q", entry.Loc)
		}
	}
}

func TestUrlEntryParseNoLongerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("u.Parse panicked instead of returning an error: %v", r)
		}
	}()

	entry := u{Loc: "https://example.com/a"}
	got, err := entry.Parse("https://example.com/a")
	if err == nil {
		t.Error("expected an error, not a silent success")
	}
	if got != nil {
		t.Errorf("Parse() = %v, want nil alongside the error", got)
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error = %v, want it to say the method is unimplemented", err)
	}
}

func sitemapServer(t *testing.T, body string, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestFetchSitemapNormalizesEveryLoc(t *testing.T) {
	srv, _ := sitemapServer(t, `<urlset>
  <url><loc>http://www.example.com/a</loc></url>
  <url><loc>http://example.com/b/</loc></url>
</urlset>`, http.StatusOK)

	host, _ := url.Parse("https://example.com/")
	got, err := fetchSitemap(context.Background(), srv.Client(), srv.URL, host)
	if err != nil {
		t.Fatalf("fetchSitemap() error: %v", err)
	}

	want := []string{"https://example.com/a", "https://example.com/b"}
	if len(got) != len(want) {
		t.Fatalf("fetchSitemap() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fetchSitemap()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFetchSitemapResolvesRelativeLocsAgainstTheHost(t *testing.T) {
	srv, _ := sitemapServer(t, `<urlset>
  <url><loc>/relative/page</loc></url>
</urlset>`, http.StatusOK)

	host, _ := url.Parse("https://example.com")
	got, err := fetchSitemap(context.Background(), srv.Client(), srv.URL, host)
	if err != nil {
		t.Fatalf("fetchSitemap() error: %v", err)
	}
	if len(got) != 1 || got[0] != "https://example.com/relative/page" {
		t.Errorf("fetchSitemap() = %v, want the relative loc resolved", got)
	}
}

func TestFetchSitemapDefaultsToHTTPSForASchemelessURL(t *testing.T) {
	srv, _ := sitemapServer(t, `<urlset><url><loc>https://example.com/a</loc></url></urlset>`, http.StatusOK)

	host, _ := url.Parse("https://example.com")
	got, err := fetchSitemap(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "http://"), host)
	if err == nil {
		t.Fatalf("expected an error: the rewritten URL points at https on a plain-HTTP test server; got %v", got)
	}
	if !strings.Contains(err.Error(), "fetching sitemap") {
		t.Errorf("error = %v, want it wrapped as a fetch failure", err)
	}
}

func TestFetchSitemapFillsInAHostlessURL(t *testing.T) {
	srv, _ := sitemapServer(t, `<urlset><url><loc>https://example.com/a</loc></url></urlset>`, http.StatusOK)

	host, _ := url.Parse("https://example.com")
	got, err := fetchSitemap(context.Background(), srv.Client(), srv.URL, host)
	if err != nil {
		t.Fatalf("fetchSitemap() error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("fetchSitemap() = %v, want one URL", got)
	}
}

func TestFetchSitemapReportsAnHTTPError(t *testing.T) {
	srv, _ := sitemapServer(t, "nope", http.StatusNotFound)

	host, _ := url.Parse("https://example.com")
	if _, err := fetchSitemap(context.Background(), srv.Client(), srv.URL, host); err == nil {
		t.Error("expected an error for a 404 sitemap")
	}
}

func TestFetchSitemapReportsAMalformedDocument(t *testing.T) {
	srv, _ := sitemapServer(t, `<urlset><url><loc>https://example.com/a</loc></urlset>`, http.StatusOK)

	host, _ := url.Parse("https://example.com")
	_, err := fetchSitemap(context.Background(), srv.Client(), srv.URL, host)
	if err == nil {
		t.Fatal("expected an error for malformed XML")
	}
	if !strings.Contains(err.Error(), "parsing sitemap") {
		t.Errorf("error = %v, want it to name the parse stage", err)
	}
}

func TestFetchSitemapSkipsUnusableLocsRatherThanFailingTheWholeSitemap(t *testing.T) {
	srv, _ := sitemapServer(t, `<urlset>
  <url><loc>https://example.com/good-one</loc></url>
  <url><loc>/login</loc></url>
  <url><loc>https://example.com/good-two</loc></url>
  <url><loc>https://example.com/skipme.pdf</loc></url>
  <url><loc>#fragment</loc></url>
  <url><loc></loc></url>
  <url><loc>ht tp://example.com/space</loc></url>
</urlset>`, http.StatusOK)

	host, _ := url.Parse("https://example.com")
	got, err := fetchSitemap(context.Background(), srv.Client(), srv.URL, host)
	if err != nil {
		t.Fatalf("fetchSitemap() error: %v; one bad loc must not discard the sitemap", err)
	}

	want := []string{
		"https://example.com/good-one",
		"https://example.com/login",
		"https://example.com/good-two",
		"https://example.com/skipme.pdf",
	}
	if len(got) != len(want) {
		t.Fatalf("fetchSitemap() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fetchSitemap()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFetchSitemapsCombinesEverySource(t *testing.T) {
	first, _ := sitemapServer(t, `<urlset><url><loc>https://example.com/a</loc></url></urlset>`, http.StatusOK)
	second, _ := sitemapServer(t, `<urlset><url><loc>https://example.com/b</loc></url></urlset>`, http.StatusOK)

	host, _ := url.Parse("https://example.com")
	got := FetchSitemaps(context.Background(), http.DefaultClient, []string{first.URL, second.URL}, host)

	if len(got) != 2 {
		t.Fatalf("FetchSitemaps() = %v, want two URLs", got)
	}
}

func TestFetchSitemapsSkipsASourceThatFails(t *testing.T) {
	ok, _ := sitemapServer(t, `<urlset><url><loc>https://example.com/good</loc></url></urlset>`, http.StatusOK)
	bad, _ := sitemapServer(t, "unavailable", http.StatusInternalServerError)

	host, _ := url.Parse("https://example.com")
	got := FetchSitemaps(context.Background(), http.DefaultClient, []string{bad.URL, ok.URL}, host)

	if len(got) != 1 || got[0] != "https://example.com/good" {
		t.Errorf("FetchSitemaps() = %v, want only the reachable sitemap's URL", got)
	}
}

func TestFetchSitemapsOnNoSources(t *testing.T) {
	host, _ := url.Parse("https://example.com")
	if got := FetchSitemaps(context.Background(), http.DefaultClient, nil, host); len(got) != 0 {
		t.Errorf("FetchSitemaps(nil) = %v, want an empty slice", got)
	}
}

func TestFetchSitemapsWhenEverySourceFails(t *testing.T) {
	bad, _ := sitemapServer(t, "down", http.StatusServiceUnavailable)
	host, _ := url.Parse("https://example.com")

	if got := FetchSitemaps(context.Background(), http.DefaultClient, []string{bad.URL}, host); len(got) != 0 {
		t.Errorf("FetchSitemaps() = %v, want an empty slice", got)
	}
}

func TestFetchSitemapsIsDeterministic(t *testing.T) {
	var idx atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := idx.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf(
			`<urlset><url><loc>https://example.com/p%d</loc></url></urlset>`, i%5)))
	}))
	defer srv.Close()

	host, _ := url.Parse("https://example.com")
	first := FetchSitemaps(context.Background(), srv.Client(), []string{srv.URL, srv.URL}, host)
	for i := 0; i < 100; i++ {
		got := FetchSitemaps(context.Background(), srv.Client(), []string{srv.URL, srv.URL}, host)
		if len(got) != len(first) {
			t.Fatalf("run %d: got %v, want %v", i, got, first)
		}
	}
}
