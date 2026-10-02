package policy

import (
	"regexp"
	"strings"
)

// RuleSet is the crawler's own hygiene table, applied after robots.txt has had
// its say: paths that are never worth a fetch no matter what a site allows, and
// file types that are never indexable.
//
// Query parameters are deliberately absent. utils.CanonicalizeUrl strips the
// pagination and search params while canonicalizing, so they are already gone
// by the time Admit evaluates a URL.
type RuleSet struct {
	DisallowPathPrefixes []string

	SkipFileExtensions []string

	WikiNoisyNamespaces []string
}

var wikiLangSubpageRE = regexp.MustCompile(`(?i)/[a-z]{2,3}(-[a-z]{2,4})?/?$`)

func DefaultRules() RuleSet {
	return RuleSet{
		DisallowPathPrefixes: []string{
			"/login", "/logout", "/register", "/signup", "/password-reset",
			"/account/", "/cart", "/checkout", "/order/", "/payment/",
			"/search", "/filter/", "/admin/", "/dashboard/", "/settings/",
			"/404", "/error/", "/maintenance", "/test/", "/print/", "/preview/", "/tag/",
		},
		SkipFileExtensions: []string{
			".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
			".zip", ".rar", ".7z", ".tar", ".gz", ".exe", ".msi", ".dmg", ".apk",
			".jpg", ".jpeg", ".png", ".gif", ".bmp", ".tiff", ".webp", ".svg",
			".mp3", ".wav", ".aac", ".ogg", ".flac",
			".mp4", ".avi", ".mov", ".wmv", ".mkv", ".flv", ".webm",
			".css", ".js", ".ico",
		},
		WikiNoisyNamespaces: []string{"/Template:", "/Help:", "/Manual:", "/Extension:"},
	}
}

// rules is the effective table. A Config built by hand rather than by
// DefaultConfig has no Rules at all, and an empty table disables every skip, so
// that case falls back to the shipped rules. A Config that sets Rules to an
// explicitly empty slice is respected as an intentional "skip nothing".
func (m *PolicyManager) rules() RuleSet {
	if m.cfg.Rules.isZero() {
		return DefaultRules()
	}
	return m.cfg.Rules
}

func (r RuleSet) isZero() bool {
	return r.DisallowPathPrefixes == nil &&
		r.SkipFileExtensions == nil &&
		r.WikiNoisyNamespaces == nil
}

func (r RuleSet) SkipsPath(p string) bool {
	cleaned := strings.Trim(p, "/")
	if cleaned == "" {
		return false
	}
	for _, prefix := range r.DisallowPathPrefixes {
		trimmed := strings.Trim(prefix, "/")
		if trimmed == "" {
			continue
		}
		if cleaned == trimmed || strings.HasPrefix(cleaned, trimmed+"/") {
			return true
		}
	}
	return false
}

func (r RuleSet) SkipsExtension(p string) bool {
	lower := strings.ToLower(p)
	for _, ext := range r.SkipFileExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func (r RuleSet) SkipsWikiLanguageSubpage(p string) bool {
	if !wikiLangSubpageRE.MatchString(p) {
		return false
	}
	for _, ns := range r.WikiNoisyNamespaces {
		if strings.Contains(p, ns) {
			return true
		}
	}
	return false
}

func IsEnglish(lang string) bool {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		return true
	}

	primary := lang
	if i := strings.IndexAny(primary, "-_"); i >= 0 {
		primary = primary[:i]
	}

	return primary == "en" || primary == "eng"
}

type RobotsRules struct {
	Allow    []string
	Disallow []string

	allow    []compiledRule
	disallow []compiledRule
}

type compiledRule struct {
	source string
	re     *regexp.Regexp
	weight int
}

func NewRobotsRules(allow, disallow []string) RobotsRules {
	r := RobotsRules{Allow: allow, Disallow: disallow}
	for _, rule := range allow {
		if c, ok := compileRobotsRule(rule); ok {
			r.allow = append(r.allow, c)
		}
	}
	for _, rule := range disallow {
		if c, ok := compileRobotsRule(rule); ok {
			r.disallow = append(r.disallow, c)
		}
	}
	return r
}

func compileRobotsRule(rule string) (compiledRule, bool) {
	rule = strings.TrimSpace(rule)
	original := rule

	if rule == "" {
		return compiledRule{}, false
	}

	anchorEnd := strings.HasSuffix(rule, "$")
	if anchorEnd {
		rule = strings.TrimSuffix(rule, "$")
	}

	optionalSlash := len(rule) > 1 && strings.HasSuffix(rule, "/")
	if optionalSlash {
		rule = strings.TrimSuffix(rule, "/")
	}

	var b strings.Builder
	b.WriteString("^")
	for _, segment := range strings.Split(rule, "*") {
		if b.Len() > 1 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(segment))
	}
	switch {
	case anchorEnd:
		b.WriteString("$")
	case optionalSlash:
		b.WriteString("(/|$)")
	}

	re, err := regexp.Compile(b.String())
	if err != nil {
		return compiledRule{}, false
	}
	return compiledRule{source: original, re: re, weight: len(original)}, true
}

func (r RobotsRules) Allows(path string) (bool, string) {
	if path == "" {
		path = "/"
	}

	best := compiledRule{}
	var allowed bool
	decided := false

	for _, rule := range r.disallow {
		if !rule.re.MatchString(path) {
			continue
		}
		if !decided || rule.weight > best.weight {
			best, allowed, decided = rule, false, true
		}
	}
	for _, rule := range r.allow {
		if !rule.re.MatchString(path) {
			continue
		}
		if !decided || rule.weight >= best.weight {
			best, allowed, decided = rule, true, true
		}
	}

	if !decided {
		return true, ""
	}
	return allowed, best.source
}

func (r RobotsRules) Refuses(path string) bool {
	allowed, _ := r.Allows(path)
	return !allowed
}
