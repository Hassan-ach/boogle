package utils

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	disallowPathPrefixes = []string{
		"/login", "/logout", "/register", "/signup", "/password-reset",
		"/account/", "/cart", "/checkout", "/order/", "/payment/",
		"/search", "/filter/", "/admin/", "/dashboard/", "/settings/",
		"/404", "/error/", "/maintenance", "/test/", "/print/", "/preview/", "/tag/",
	}

	disallowQueryParams = []string{
		"sort", "page", "filter", "q", "search",
	}

	skipFileExtensions = []string{
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".zip", ".rar", ".7z", ".tar", ".gz", ".exe", ".msi", ".dmg", ".apk",
		".jpg", ".jpeg", ".png", ".gif", ".bmp", ".tiff", ".webp", ".svg",
		".mp3", ".wav", ".aac", ".ogg", ".flac",
		".mp4", ".avi", ".mov", ".wmv", ".mkv", ".flv", ".webm",
		".css", ".js", ".ico",
	}

	langSubdomainDomains = []string{
		"wikipedia.org",
		"wikibooks.org",
		"wikivoyage.org",
	}
	// Wiki-specific: blocks /Template:Foo/xx/, /Help:Bar/en/, etc.
	wikiLangSubpageRE   = regexp.MustCompile(`(?i)/[a-z]{2,3}(-[a-z]{2,4})?/?$`)
	wikiNoisyNamespaces = []string{"/Template:", "/Help:", "/Manual:", "/Extension:"}
)

func NormalizeUrl(raw string, baseHost string) (string, bool) {
	if !utf8.ValidString(raw) || strings.HasPrefix(raw, "#") {
		return "", false
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}

	if shouldSkipByPath(u.Path) {
		return "", false
	}

	if shouldSkipByExtension(u.Path) {
		return "", false
	}

	if shouldSkipWikiLanguageSubpage(u.Path) {
		return "", false
	}

	normalizeURLParts(u, baseHost)

	return u.String(), true
}

func shouldSkipByPath(path string) bool {
	for _, prefix := range disallowPathPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func shouldSkipByExtension(path string) bool {
	pathLower := strings.ToLower(path)
	for _, ext := range skipFileExtensions {
		if strings.HasSuffix(pathLower, ext) {
			return true
		}
	}
	return false
}

func isValidQueryParam(q string) bool {
	return !slices.Contains(disallowQueryParams, q)
}

func shouldSkipWikiLanguageSubpage(path string) bool {
	if !wikiLangSubpageRE.MatchString(path) {
		return false
	}
	for _, ns := range wikiNoisyNamespaces {
		if strings.Contains(path, ns) {
			return true
		}
	}
	return false
}

func normalizeURLParts(u *url.URL, baseHost string) {
	// Force HTTPS
	u.Scheme = "https"

	// Fill host if relative URL
	if u.Host == "" && baseHost != "" {
		u.Host = baseHost
	}

	// Clean host
	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")

	forceEnglishSubdomain(u)

	// Remove fragment
	u.Fragment = ""

	// Canonicalize query: sorted keys
	if u.RawQuery != "" {
		q := u.Query()
		keys := make([]string, 0, len(q))
		for k := range q {
			if isValidQueryParam(k) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)

		sorted := url.Values{}
		for _, k := range keys {
			sorted[k] = q[k]
		}
		u.RawQuery = sorted.Encode()
	}

	// Remove trailing slash
	if len(u.Query()) == 0 &&
		u.Path != "" &&
		strings.HasSuffix(u.Path, "/") &&
		!strings.Contains(u.Path, ".") {
		u.Path = strings.TrimSuffix(u.Path, "/")
	}
}

func NormalizeUrls(raws []string, baseHost string) []string {
	result := make([]string, 0, len(raws))
	for _, raw := range raws {
		if norm, ok := NormalizeUrl(raw, baseHost); ok {
			result = append(result, norm)
		}
	}
	return result
}

func forceEnglishSubdomain(u *url.URL) {
	u.Host = strings.ToLower(u.Host)
	u.Host = strings.TrimPrefix(u.Host, "www.")

	// Only act on known wiki-family domains
	isKnownLangDomain := false
	for _, d := range langSubdomainDomains {
		if strings.HasSuffix(u.Host, d) {
			isKnownLangDomain = true
			break
		}
	}
	if !isKnownLangDomain {
		return
	}

	hostParts := strings.Split(u.Host, ".")
	if len(hostParts) < 3 {
		return
	}

	potentialLang := hostParts[0]
	rest := strings.Join(hostParts[1:], ".")

	if potentialLang == "en" {
		return // already English
	}

	isLang := len(potentialLang) >= 2 && len(potentialLang) <= 5 &&
		regexp.MustCompile(`^[a-z]+(-[a-z]+)?$`).MatchString(potentialLang)

	if isLang {
		u.Host = "en." + rest
	}
}

func CheckURLExists(rawURL string) bool {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}

	resp, err := client.Head(rawURL)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func ValidateLinks(links []string, disallowed []string) []string {
	normUrls := NewSet[string]()
	for _, x := range links {
		ur, err := url.Parse(x)
		if err != nil || isDisallowed(ur.Path, disallowed) {
			continue
		}
		normUrls.Add(ur.String())
	}
	return normUrls.GetAll()
}

func isDisallowed(path string, disallowed []string) bool {
	for _, d := range disallowed {
		// detect if pattern looks like regex
		if strings.ContainsAny(d, `.^$*+?[]|()`) {
			matched, err := regexp.MatchString(d, path)
			if err != nil {
				continue
			}
			if matched {
				return true
			}
		} else {
			if strings.HasPrefix(path, d) {
				return true
			}
		}
	}
	return false
}

func IsDisallowed(path string, disallowed []string) bool {
	for _, d := range disallowed {
		if d == "" {
			continue
		}
		// Check for regex or prefix pattern
		if strings.ContainsAny(d, `.^$*+?[]|()`) {
			if matched, _ := regexp.MatchString(d, path); matched {
				return true
			}
		} else {
			if strings.HasPrefix(path, d) {
				return true
			}
		}
	}
	return false
}
