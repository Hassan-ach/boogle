package entity

import (
	"time"
)

type MetaData struct {
	// OgURL is the publisher's claimed canonical URL (`<meta property="og:url">`).
	// It is display metadata only. Page identity is Page.URL, which is always the
	// URL the spider fetched, because a page must not be able to choose which
	// row of the index it overwrites.
	OgURL       string    `json:"ogUrl,omitempty"`
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

type Page struct {
	// URL is storage identity: the URL that was fetched, not anything the page
	// asserts about itself.
	URL string `json:"url"`
	MetaData
	StatusCode int
	HTML       []byte
	Images     []string
	Links      []string
}
