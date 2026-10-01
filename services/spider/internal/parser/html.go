package parser

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"

	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

type htmlCollector struct {
	Links      []string
	Imags      []string
	TextBuffer strings.Builder
	Meta       entity.MetaData
	BaseURL    *url.URL
}

func newHtmlCollector(baseURL *url.URL) *htmlCollector {
	return &htmlCollector{
		BaseURL: baseURL,
	}
}

func (c *htmlCollector) conllectText(text string) {
	s := strings.TrimSpace(text)
	if s == "" {
		return
	}

	if c.TextBuffer.Len() > 0 {
		c.TextBuffer.WriteByte(' ')
	}

	c.TextBuffer.WriteString(s)
}

func (c *htmlCollector) Visit(n *html.Node) {
	if n.Type != html.ElementNode {
		if n.Type == html.TextNode {
			c.conllectText(n.Data)
		}
		return
	}

	switch n.Data {
	case "script", "style":
		return
	case "meta":
		c.mergeMeta(extrantMeta(n))
	case "link":
		if isIconLink(n) {
			c.Meta.Icons = append(c.Meta.Icons, getAttr(n, "href"))
		}
	case "a":
		c.maybeAddLink(getAttr(n, "href"))
	case "img":
		c.maybeAddImage(getAttr(n, "src"))
	case "title":
		if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
			c.Meta.Title = strings.TrimSpace(n.FirstChild.Data)
		}
	}
}

func (c *htmlCollector) mergeMeta(other entity.MetaData) entity.MetaData {
	if other.URL != "" {
		c.Meta.URL = other.URL
	}
	if other.Title != "" {
		c.Meta.Title = other.Title
	}
	if other.Description != "" {
		c.Meta.Description = other.Description
	}
	if other.Type != "" {
		c.Meta.Type = other.Type
	}
	if other.SiteName != "" {
		c.Meta.SiteName = other.SiteName
	}
	if other.Locale != "" {
		c.Meta.Locale = other.Locale
	}
	c.Meta.Keywords = append(c.Meta.Keywords, other.Keywords...)
	c.Meta.Icons = append(c.Meta.Icons, other.Icons...)
	return c.Meta
}

func (c *htmlCollector) maybeAddLink(rawURL string) {
	if _, err := url.Parse(rawURL); err == nil {
		u, ok := utils.CanonicalizeUrl(rawURL, "")
		if !ok {
			return
		}
		c.Links = append(c.Links, u)
	}
}

func (c *htmlCollector) maybeAddImage(src string) {
	if src == "" {
		return
	}
	if _, err := url.Parse(src); err == nil {
		c.Imags = append(c.Imags, src)
	}
}
