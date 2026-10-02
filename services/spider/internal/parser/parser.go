package parser

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	c.Meta.Lang = documentLanguage(doc)
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
			utils.CanonicalizeUrls(c.Links, u.Host)).GetAll(),
		Images: utils.NewSetFromSlice(c.Imags).GetAll(),
	}, nil
}


