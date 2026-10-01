package parser

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"

	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

type u struct {
	Loc string `xml:"loc"`
}

// Parse satisfies the shape a caller might expect from a sitemap entry, but
// there is no meaningful per-entry parse: the value is already a plain string
// from the XML decoder. It used to be `panic("unimplemented")`, which compiled
// fine because nothing called it and would have taken the process down the day
// something did.
func (u u) Parse(raw string) (any, error) {
	return nil, fmt.Errorf("sitemap entry Parse is not implemented; read Loc directly (raw=%q)", raw)
}

type SiteMaps struct {
	Urls []u `xml:"url"`
}

func parseSitemap(file []byte) (*SiteMaps, error) {
	var sitemap SiteMaps
	if err := xml.Unmarshal(file, &sitemap); err != nil {
		return nil, err
	}

	return &sitemap, nil
}

func fetchSitemap(ctx context.Context, client *http.Client, sitemapURL string, host *url.URL) ([]string, error) {
	siteUrl, err := url.Parse(sitemapURL)
	if err != nil {
		return nil, fmt.Errorf("fetching sitemap: invalid URL %s: %w", sitemapURL, err)
	}
	if siteUrl.Scheme == "" {
		siteUrl.Scheme = "https"
	}
	if siteUrl.Host == "" {
		if host == nil {
			return nil, fmt.Errorf("fetching sitemap: %s has no host and none was supplied", sitemapURL)
		}
		siteUrl.Host = host.Host
	}

	res, err := utils.GetReq(ctx, client, siteUrl.String(), utils.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("fetching sitemap: %w", err)
	}
	// The old code parsed whatever came back and ignored the status, so a sitemap
	// that answered 404 with an HTML error page was reported as a parse failure --
	// which reads like a broken sitemap file and is actually a missing one.
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching sitemap: %s returned %d", sitemapURL, res.StatusCode)
	}

	d, err := parseSitemap(res.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing sitemap: %w", err)
	}

	// A sitemap routinely contains a handful of entries this crawler will not
	// take: a /login link, a PDF, a bare fragment. Returning an error for the
	// first one used to discard every good URL in the file.
	r := make([]string, 0, len(d.Urls))
	for _, entry := range d.Urls {
		if entry.Loc == "" {
			continue
		}
		x, ok := utils.CanonicalizeUrl(entry.Loc, host.Host)
		if !ok {
			continue
		}
		r = append(r, x)
	}

	return r, nil
}

// FetchSitemaps expands the sitemap URLs a host advertises into page URLs.
//
// A sitemap that cannot be read is skipped rather than returned as an error, and
// that is a deliberate choice about severity: a missing or malformed sitemap costs
// the discovery of some URLs and nothing else. It used to be fatal to the whole
// host, which is how a site with one broken sitemap reference became a site that
// could not be crawled at all.
//
// The context is honoured so that a crawl shutting down does not wait on a
// sitemap fetch, and so that the caller can bound a sitemap that is taking
// minutes to serve a file with fifty thousand entries in it.
func FetchSitemaps(ctx context.Context, client *http.Client, s []string, host *url.URL) []string {
	r := make([]string, 0)
	for _, sitemapURL := range s {
		if siteUrls, err := fetchSitemap(ctx, client, sitemapURL, host); err == nil {
			r = append(r, siteUrls...)
		}
	}
	return r
}
