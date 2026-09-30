package parser

import (
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
		m.URL = content
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

// findHtmlNode traverses top-level nodes to find the <html> ElementNode
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

// documentLanguage reports the language a document declares for itself, by
// reading the lang or xml:lang attribute off the <html> element.
//
// It reports rather than decides. It used to return a skip boolean, and
// ParseHTML turned that into an error, so every non-English page logged "failed
// to fetch and parse page" and was indistinguishable from a network failure --
// and neither was countable. The policy manager makes the call now; this only
// supplies the evidence.
//
// An absent attribute is reported as the empty string, which policy.IsEnglish
// reads as "no claim made" rather than "not English". Most real pages omit it.
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
