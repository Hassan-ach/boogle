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
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching sitemap: %s returned %d", sitemapURL, res.StatusCode)
	}

	d, err := parseSitemap(res.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing sitemap: %w", err)
	}

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

func FetchSitemaps(ctx context.Context, client *http.Client, s []string, host *url.URL) []string {
	r := make([]string, 0)
	for _, sitemapURL := range s {
		if siteUrls, err := fetchSitemap(ctx, client, sitemapURL, host); err == nil {
			r = append(r, siteUrls...)
		}
	}
	return r
}
