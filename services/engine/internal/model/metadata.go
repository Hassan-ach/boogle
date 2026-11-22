package model

import "time"

type MetaData struct {
	// OgURL is the publisher's claimed canonical URL. It is display metadata
	// only; Page.URL is the identity the crawler stored.
	OgURL       string    `json:"ogUrl,omitempty"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Type        string    `json:"type,omitempty"`
	SiteName    string    `json:"siteName,omitempty"`
	Locale      string    `json:"locale,omitempty"`
	Keywords    []string  `json:"keywords,omitempty"`
	Icons       []string  `json:"icons,omitempty"`
	CrawledAt   time.Time `json:"crawledAt"`
}
