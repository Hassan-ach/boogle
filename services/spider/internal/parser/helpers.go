package parser

import (
	"net/url"
	"strings"

	"github.com/Hassan-ach/boogle/services/spider/internal/entity"

	"golang.org/x/net/html"
)

func extrantMeta(n *html.Node) entity.MetaData {
	prop := getMetaProperty(n)
	content := getMetaContent(n)
	if content == "" {
		return entity.MetaData{}
	}
	var m entity.MetaData
	switch prop {
	case "url":
		m.OgURL = content
	case "title":
		m.Title = content
	case "description":
		m.Description = content
	case "type":
		m.Type = content
	case "site_name":
		m.SiteName = content
	case "locale":
		m.Locale = content
	case "keywords":
		m.Keywords = strings.Split(content, ",")
		for i := range m.Keywords {
			m.Keywords[i] = strings.TrimSpace(m.Keywords[i])
		}
	}
	return m
}

func findHtmlNode(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && strings.EqualFold(n.Data, "html") {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if res := findHtmlNode(c); res != nil {
			return res
		}
	}
	return nil
}

func documentLanguage(root *html.Node) string {
	htmlNode := findHtmlNode(root)
	if htmlNode == nil {
		return ""
	}
	for _, attr := range htmlNode.Attr {
		key := strings.ToLower(attr.Key)
		if key == "lang" || key == "xml:lang" {
			return strings.TrimSpace(attr.Val)
		}
	}
	return ""
}

func traverse(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		traverse(child, visit)
	}
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func isIconLink(n *html.Node) bool {
	for _, v := range n.Attr {
		if v.Key == "rel" && v.Val == "icon" {
			return true
		}
	}
	return false
}

func getMetaProperty(n *html.Node) string {
	for _, a := range n.Attr {
		if a.Key == "property" {
			if strings.HasPrefix(a.Val, "og:") {
				return a.Val[3:]
			}
		}
		if a.Key == "name" {
			return a.Val
		}
	}
	return ""
}

func getMetaContent(n *html.Node) string {
	for _, a := range n.Attr {
		if a.Key == "content" {
			return a.Val
		}
	}

	return ""
}

// sameHost reports whether raw is an absolute HTTP(S) URL on the same host as
// base. Used to keep a cross-origin `og:url` out of a page's metadata: it cannot
// change storage identity any more, but it must not be presented as this page's
// canonical address either.
func sameHost(base *url.URL, raw string) bool {
	if base == nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if !strings.EqualFold(u.Hostname(), base.Hostname()) {
		return false
	}
	// A downgrade or a different port is a different origin even on the same
	// hostname, and must not be presented as this page's canonical address.
	if !strings.EqualFold(u.Scheme, base.Scheme) {
		return false
	}
	return effectivePort(u) == effectivePort(base)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}
