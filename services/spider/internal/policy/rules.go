package policy

import (
	"regexp"
	"strings"
)

type RuleSet struct {
	DisallowPathPrefixes []string

	DisallowQueryParams []string

	SkipFileExtensions []string

	LangSubdomainDomains []string

	WikiNoisyNamespaces []string
}

var wikiLangSubpageRE = regexp.MustCompile(`(?i)/[a-z]{2,3}(-[a-z]{2,4})?/?$`)

var languageSubtagRE = regexp.MustCompile(`^[a-z]{2,8}(-[a-z]{2,8}){0,2}$`)

func DefaultRules() RuleSet {
	return RuleSet{
		DisallowPathPrefixes: []string{
			"/login", "/logout", "/register", "/signup", "/password-reset",
			"/account/", "/cart", "/checkout", "/order/", "/payment/",
			"/search", "/filter/", "/admin/", "/dashboard/", "/settings/",
			"/404", "/error/", "/maintenance", "/test/", "/print/", "/preview/", "/tag/",
		},
		DisallowQueryParams: []string{
			"sort", "page", "filter", "q", "search",
		},
		SkipFileExtensions: []string{
			".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
			".zip", ".rar", ".7z", ".tar", ".gz", ".exe", ".msi", ".dmg", ".apk",
			".jpg", ".jpeg", ".png", ".gif", ".bmp", ".tiff", ".webp", ".svg",
			".mp3", ".wav", ".aac", ".ogg", ".flac",
			".mp4", ".avi", ".mov", ".wmv", ".mkv", ".flv", ".webm",
			".css", ".js", ".ico",
		},
		LangSubdomainDomains: []string{
			"wikipedia.org",
			"wikibooks.org",
			"wikivoyage.org",
		},
		WikiNoisyNamespaces: []string{"/Template:", "/Help:", "/Manual:", "/Extension:"},
	}
}

func (m *PolicyManager) rules() RuleSet {
	if m.cfg.Rules.isZero() {
		return DefaultRules()
	}
	return m.cfg.Rules
}

func (r RuleSet) isZero() bool {
	return r.DisallowPathPrefixes == nil &&
		r.DisallowQueryParams == nil &&
		r.SkipFileExtensions == nil &&
		r.LangSubdomainDomains == nil &&
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

func (r RuleSet) DropsQueryParam(q string) bool {
	for _, drop := range r.DisallowQueryParams {
		if q == drop {
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

func (r RuleSet) FoldsLanguageSubdomain(host string) string {
	host = strings.ToLower(host)
	host = strings.TrimPrefix(host, "www.")

	isKnownLangDomain := false
	for _, d := range r.LangSubdomainDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			isKnownLangDomain = true
			break
		}
	}
	if !isKnownLangDomain {
		return host
	}

	parts := strings.Split(host, ".")
	if len(parts) < 3 {
		return host
	}

	potentialLang := parts[0]
	if potentialLang == "en" {
		return host
	}
	if !languageSubtagRE.MatchString(potentialLang) {
		return host
	}
	return "en." + strings.Join(parts[1:], ".")
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
