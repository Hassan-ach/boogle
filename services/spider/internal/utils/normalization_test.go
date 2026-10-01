package utils

import (
	"testing"
)

func TestNormalizeUrl(t *testing.T) {
	tests := []struct {
		name        string
		rawURL      string
		baseHost    string
		expectedURL string
		expectedOK  bool
	}{
		{
			name:        "valid absolute URL",
			rawURL:      "http://example.com/page",
			baseHost:    "example.com",
			expectedURL: "https://example.com/page",
			expectedOK:  true,
		},
		{
			name:        "relative URL resolution",
			rawURL:      "/wiki/Golang",
			baseHost:    "en.wikipedia.org",
			expectedURL: "https://en.wikipedia.org/wiki/Golang",
			expectedOK:  true,
		},
		{
			name:        "keeps a path the rules would refuse",
			rawURL:      "http://example.com/login",
			baseHost:    "example.com",
			expectedURL: "https://example.com/login",
			expectedOK:  true,
		},
		{
			name:        "keeps an extension the rules would refuse",
			rawURL:      "http://example.com/document.pdf",
			baseHost:    "example.com",
			expectedURL: "https://example.com/document.pdf",
			expectedOK:  true,
		},
		{
			name:        "strip fragment",
			rawURL:      "http://example.com/page#section1",
			baseHost:    "example.com",
			expectedURL: "https://example.com/page",
			expectedOK:  true,
		},
		{
			name:        "force HTTPS scheme",
			rawURL:      "http://example.com/item",
			baseHost:    "example.com",
			expectedURL: "https://example.com/item",
			expectedOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotOK := NormalizeUrl(tt.rawURL, tt.baseHost)
			if gotOK != tt.expectedOK {
				t.Errorf("NormalizeUrl() ok = %v, expected %v", gotOK, tt.expectedOK)
			}
			if gotURL != tt.expectedURL {
				t.Errorf("NormalizeUrl() url = %v, expected %v", gotURL, tt.expectedURL)
			}
		})
	}
}

func TestIsDisallowed(t *testing.T) {
	disallowed := []string{"/admin/", "/private", `^/secret/.*`}

	tests := []struct {
		path     string
		expected bool
	}{
		{"/admin/users", true},
		{"/private/data", true},
		{"/secret/keys", true},
		{"/public/index.html", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := IsDisallowed(tt.path, disallowed)
			if got != tt.expected {
				t.Errorf("IsDisallowed(%s) = %v, expected %v", tt.path, got, tt.expected)
			}
		})
	}
}
