package entity

import (
	"time"
)

type Host struct {
	MaxRetry        int
	MaxPages        int
	PagesCrawled    int
	Delay           int
	Name            string
	AllowedUrls     []string
	NotAllowedPaths []string
}
type MetaData struct {
	URL         string    `json:"url"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Type        string    `json:"type,omitempty"`
	SiteName    string    `json:"siteName,omitempty"`
	Locale      string    `json:"locale,omitempty"`
	Keywords    []string  `json:"keywords,omitempty"`
	Icons       []string  `json:"icons,omitempty"`
	Lang        string    `json:"lang,omitempty"`
	CrawledAt   time.Time `json:"crawledAt"`
}

type Robots struct {
	Allow      []string
	Disallow   []string
	SiteMaps   []string
	CrawlDelay int
}

type Page struct {
	MetaData
	StatusCode int
	HTML       []byte
	Images     []string
	Links      []string
}
