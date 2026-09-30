package utils

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
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

	// languageSubtagRE matches an IETF BCP-47 language tag: two to three
	// subtags of two to eight alphanumerics, at least the first of them letters.
	// The old check was "two to five characters, letters, one optional hyphen",
	// which missed zh-yue, be-tarask, zh-min-nan and every other language
	// Wikipedia publishes under, so those mirrors got crawled in full instead of
	// being folded onto en.
	languageSubtagRE = regexp.MustCompile(`^[a-z]{2,8}(-[a-z]{2,8}){0,2}$`)
)

func NormalizeUrl(raw string, baseHost string) (string, bool) {
	if !utf8.ValidString(raw) || strings.HasPrefix(raw, "#") {
		return "", false
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}

	// Resolve "." and ".." segments and collapse repeated slashes before any
	// decision is made about the path. "/a/./b", "/a//b" and "/a/b" are the same
	// document, and keying them separately splits a page's word counts and
	// PageRank three ways. The skip checks then have to run on the cleaned path,
	// or "/admin/../public" would slip past the /admin rule.
	//
	// path.Clean also drops a trailing slash, which is load bearing for servers
	// that distinguish "/a" from "/a/", so it is recorded and re-applied later.
	hadTrailingSlash := strings.HasSuffix(u.Path, "/")
	u.Path = path.Clean("/" + u.Path)
	if u.Path == "/" {
		// The site root renders without a trailing slash, so "https://x" and
		// "https://x/" cannot end up as two keys for the home page.
		u.Path = ""
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

	normalizeURLParts(u, baseHost, hadTrailingSlash)

	return u.String(), true
}

// shouldSkipByPath reports whether a path is one the crawler should not index.
//
// The comparison is on path *segments*, not on a raw string prefix. A plain
// HasPrefix meant "/cart" also swallowed "/cartoon" and "/cartoonography",
// "/search" swallowed "/searching" and "/search-archive", and "/login"
// swallowed "/logins-archive" -- all ordinary content pages that then never
// reached the index, with no error anywhere to say why.
func shouldSkipByPath(path string) bool {
	cleaned := strings.Trim(path, "/")
	if cleaned == "" {
		return false
	}
	for _, prefix := range disallowPathPrefixes {
		trimmed := strings.Trim(prefix, "/")
		if trimmed == "" {
			continue
		}
		// Match when the rule is a whole leading path segment. "/admin" covers
		// "/admin" and "/admin/users" but not "/administration".
		if cleaned == trimmed || strings.HasPrefix(cleaned, trimmed+"/") {
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

func normalizeURLParts(u *url.URL, baseHost string, hadTrailingSlash bool) {
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

	// The trailing slash is significant to some servers -- "/a" and "/a/" can be
	// different documents -- but not all, and path.Clean has already thrown it
	// away. Put it back only where it is meaningful: when a query survived, or
	// when the path looks like a filename, in which case the slash denotes a
	// directory listing rather than the document itself.
	if hadTrailingSlash && u.Path != "" && !strings.HasSuffix(u.Path, "/") {
		if len(u.Query()) > 0 || strings.Contains(u.Path, ".") {
			u.Path += "/"
		}
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

	if languageSubtagRE.MatchString(potentialLang) {
		u.Host = "en." + rest
	}
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

// IsDisallowed reports whether a path is blocked by any of the given rules.
//
// A rule containing a regex metacharacter is treated as a regular expression,
// otherwise it is a path prefix. Two rules that used to live here as separate
// copies of this function disagreed: the unexported one had no guard for an
// empty rule, and strings.HasPrefix(p, "") is true for every p, so a single
// blank line in a robots.txt file blocked the entire host.
func IsDisallowed(path string, disallowed []string) bool {
	for _, d := range disallowed {
		if d == "" {
			continue
		}
		if strings.ContainsAny(d, `.^$*+?[]|()`) {
			// An unparseable pattern is skipped rather than allowed through:
			// failing open on a malformed rule would crawl something an
			// operator explicitly asked not to be crawled.
			if matched, err := regexp.MatchString(d, path); err == nil && matched {
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

// isDisallowed is the private alias kept for the callers inside this package.
// It delegates rather than reimplementing, so the two cannot drift apart again.
func isDisallowed(p string, disallowed []string) bool {
	return IsDisallowed(p, disallowed)
}
