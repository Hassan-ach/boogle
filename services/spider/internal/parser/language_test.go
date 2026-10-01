package parser

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
)

func TestParseHTMLReportsDeclaredLanguage(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{
			name: "html lang is reported verbatim",
			html: `<html lang="de"><body><p>hallo</p></body></html>`,
			want: "de",
		},
		{
			name: "a regional variant is reported in full, not truncated",
			html: `<html lang="pt-BR"><body><p>ola</p></body></html>`,
			want: "pt-BR",
		},
		{
			name: "xml:lang is read too",
			html: `<html xml:lang="fr"><body><p>bonjour</p></body></html>`,
			want: "fr",
		},
		{
			name: "surrounding whitespace is trimmed",
			html: "<html lang=\"  en-GB  \"><body><p>hi</p></body></html>",
			want: "en-GB",
		},
		{
			name: "an absent attribute is reported as empty, not guessed",
			html: `<html><body><p>no claim made</p></body></html>`,
			want: "",
		},
		{
			name: "an empty attribute is reported as empty",
			html: `<html lang=""><body><p>nothing</p></body></html>`,
			want: "",
		},
	}

	p := NewParser(nil, newTestLogger())
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, err := p.ParseHTML(bytes.NewBufferString(tc.html), "https://example.com/")
			if err != nil {
				t.Fatalf("ParseHTML returned an error for a well-formed document: %v", err)
			}
			if page.Lang != tc.want {
				t.Errorf("Lang = %q, want %q", page.Lang, tc.want)
			}
		})
	}
}

func TestParseHTMLDoesNotRefuseNonEnglish(t *testing.T) {
	p := NewParser(nil, newTestLogger())

	html := `<html lang="de"><head><title>Titel</title></head>` +
		`<body><a href="/a">a</a><a href="/b">b</a><p>Ein Absatz auf Deutsch.</p></body></html>`

	page, err := p.ParseHTML(bytes.NewBufferString(html), "https://example.com/")
	if err != nil {
		t.Fatalf("ParseHTML refused a non-English document: %v", err)
	}
	if page.Title != "Titel" {
		t.Errorf("Title = %q, want %q", page.Title, "Titel")
	}
	if len(page.Links) != 2 {
		t.Errorf("extracted %d links, want 2: %v", len(page.Links), page.Links)
	}
	if !strings.Contains(page.Description, "Ein Absatz auf Deutsch.") {
		t.Errorf("Description = %q, want it to contain the paragraph text", page.Description)
	}

	if policy.IsEnglish(page.Lang) {
		t.Error("policy.IsEnglish accepted a document declaring lang=de")
	}
}

func TestParserAndPolicyAgreeOnEnglish(t *testing.T) {
	p := NewParser(nil, newTestLogger())

	accepted := []string{"en", "en-US", "en-GB", ""}
	declined := []string{"de", "fr", "ja", "ru", "ar"}

	for _, lang := range accepted {
		html := `<html lang="` + lang + `"><body><p>text</p></body></html>`
		page, err := p.ParseHTML(bytes.NewBufferString(html), "https://example.com/")
		if err != nil {
			t.Fatalf("lang=%q: %v", lang, err)
		}
		if !policy.IsEnglish(page.Lang) {
			t.Errorf("lang=%q: reported %q and was declined, want accepted", lang, page.Lang)
		}
	}

	for _, lang := range declined {
		html := `<html lang="` + lang + `"><body><p>text</p></body></html>`
		page, err := p.ParseHTML(bytes.NewBufferString(html), "https://example.com/")
		if err != nil {
			t.Fatalf("lang=%q: %v", lang, err)
		}
		if policy.IsEnglish(page.Lang) {
			t.Errorf("lang=%q: reported %q and was accepted, want declined", lang, page.Lang)
		}
	}
}

func TestParseHTMLWithoutHTMLRoot(t *testing.T) {
	p := NewParser(nil, newTestLogger())

	page, err := p.ParseHTML(bytes.NewBufferString(`<p>bare fragment</p>`), "https://example.com/")
	if err != nil {
		t.Fatalf("ParseHTML on a fragment: %v", err)
	}
	if page.Lang != "" {
		t.Errorf("Lang = %q, want empty when there is no html element", page.Lang)
	}
	if !policy.IsEnglish(page.Lang) {
		t.Error("a document that makes no language claim must not be declined")
	}
}
