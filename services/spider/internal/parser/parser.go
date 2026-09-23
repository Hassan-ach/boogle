package parser

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"

	"golang.org/x/net/html"
)

type Parser struct {
	Client *http.Client
	log    *slog.Logger
}

func NewParser(client *http.Client, logger *utils.Logger) *Parser {
	return &Parser{
		Client: client,
		log:    logger.With("component", "parser"),
	}
}

func (p *Parser) ParseHTML(r io.Reader, baseURL string) (*entity.Page, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}

	u, _ := url.Parse(baseURL)

	c := newHtmlCollector(u)
	if skipNonEnglish(doc) {
		return nil, fmt.Errorf("non-English content detected, skipping")
	}
	traverse(doc, c.Visit)

	desc := strings.TrimSpace(c.TextBuffer.String())
	if len(desc) > 300 {
		desc = desc[:300]
	}

	if c.Meta.Description == "" {
		c.Meta.Description = desc
	}
	c.Meta.CrawledAt = time.Now()

	p.log.Info(
		"",
		"links_found",
		len(c.Links),
		"images_found",
		len(c.Imags),
	)

	return &entity.Page{
		MetaData: c.Meta,
		Links: utils.NewSetFromSlice(
			utils.NormalizeUrls(c.Links, u.Host)).GetAll(),
		Images: utils.NewSetFromSlice(c.Imags).GetAll(),
	}, nil
}

func (p *Parser) ParseRobots(txt, ua string) *entity.Robots {
	r := &entity.Robots{
		CrawlDelay: 5,
	}

	if ua == "" {
		ua = "*"
	}

	var uaActive bool
	for _, line := range strings.Split(txt, "\n") {

		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		lower := strings.ToLower(line)

		switch {
		case strings.HasPrefix(lower, "user-agent:"):
			userAgent := strings.TrimSpace(line[11:])
			uaActive = (userAgent == ua || userAgent == "*")
		case uaActive:
			switch {
			case strings.HasPrefix(lower, "disallow:"):
				rule := strings.TrimSpace(line[9:])
				r.Disallow = append(r.Disallow, rule)
			case strings.HasPrefix(lower, "allow:"):
				rule := strings.TrimSpace(line[6:])
				r.Allow = append(r.Allow, rule)
			case strings.HasPrefix(lower, "crawl-delay:"):
				if d, err := strconv.Atoi(strings.TrimSpace(line[12:])); err == nil && d > 0 {
					r.CrawlDelay = d
				}
			}

		case strings.HasPrefix(lower, "sitemap:"):
			sitemapURL := strings.TrimSpace(line[8:])
			r.SiteMaps = append(r.SiteMaps, sitemapURL)
		}
	}
	return r
}
