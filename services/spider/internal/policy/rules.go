package policy

import (
	"regexp"
	"strings"
)

// This file collects the rules that decide what a crawler is willing to fetch.
// The rest of them are relocated from utils in the centralisation phase; the
// language rule lands here first because the parser has to stop making the
// decision before the manager can own it.
//
// # Why the tables are data and not code
//
// Every one of these lists is a judgement about what a general-purpose web index
// wants, not a fact about URLs. "/admin" is not disallowed by any property of the
// string "/admin"; it is disallowed because indexing a login form is worthless
// and fetching it is impolite. That makes them configuration, and it means the
// operators who disagree get an answer that does not involve a recompile.
//
// A RuleSet is a value, so a caller can take the default, adjust one list, and
// hand it to the manager. It is not read from the environment directly, because
// a list of URL fragments in an environment variable is unreadable and
// unvalidatable; the config loader in the service builds one.

// RuleSet is the set of tables a manager consults.
type RuleSet struct {
	// DisallowPathPrefixes are paths no document is indexed from. Compared
	// against whole path *segments*, not as string prefixes.
	DisallowPathPrefixes []string

	// DisallowQueryParams are query keys dropped during canonicalisation. A
	// parameter that only varies the view -- sort order, page number, a search
	// term -- makes one document into an unbounded number of near-identical
	// keys, each of which then competes for the same words.
	//
	// These are dropped rather than refused, so they are not a skip rule and
	// belong to canonicalisation. The table lives here because it is the same
	// judgement as the rest of these, and splitting one judgement across two
	// packages is how it ends up owned by neither.
	DisallowQueryParams []string

	// SkipFileExtensions are paths ending in one of these are not crawled at
	// all. Fetching a PDF to decide not to index it costs a full transfer, and
	// the parser has no way to extract text from one anyway.
	SkipFileExtensions []string

	// LangSubdomainDomains are hosts whose subdomains are language codes
	// ("fr.wikipedia.org"), which are folded onto English rather than crawled
	// separately.
	LangSubdomainDomains []string

	// WikiNoisyNamespaces are wiki page-namespace prefixes whose pages are
	// templates, help text and module source rather than articles. Combined
	// with WikiLangSubpage they identify the translated copies of those
	// namespaces, which are the largest single source of low-value pages in a
	// wiki crawl.
	WikiNoisyNamespaces []string
}

// wikiLangSubpageRE matches a path ending in a BCP-47 language subpage, e.g.
// "/Template:Infobox/fr/". It is deliberately not anchored to the start of the
// path, because the interesting part is the combination of a noisy namespace
// earlier in the path and a language subpage at the end.
var wikiLangSubpageRE = regexp.MustCompile(`(?i)/[a-z]{2,3}(-[a-z]{2,4})?/?$`)

// languageSubtagRE matches an IETF BCP-47 language tag: two to three subtags of
// two to eight alphanumerics, at least the first of them letters. The old check
// was "two to five characters, letters, one optional hyphen", which missed
// zh-yue, be-tarask, zh-min-nan and every other language Wikipedia publishes
// under, so those mirrors got crawled in full instead of being folded onto en.
var languageSubtagRE = regexp.MustCompile(`^[a-z]{2,8}(-[a-z]{2,8}){0,2}$`)

// DefaultRules is the shipped rule set.
//
// The values are carried over unchanged from the tables in utils, because
// changing them and moving them at the same time would make it impossible to
// tell which of the two effects a crawl-volume change came from. Whether each
// entry is still the right judgement is a separate question, and a real one --
// "/tag/" is here because tag index pages are near-duplicates of each other,
// which is a defensible call and also one that discards pages some sites use for
// navigation.
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

// rules returns the manager's rule set, substituting the default for a nil one.
//
// A nil RuleSet is reachable by any caller that builds a Config literal instead
// of calling DefaultConfig, and a nil one must not read as "allow everything":
// that would silently disable every rule in this file for a caller who simply
// left a field unset, which is exactly the class of bug that turns a strict
// crawler into an impolite one.
func (m *PolicyManager) rules() RuleSet {
	if m.cfg.Rules.isZero() {
		return DefaultRules()
	}
	return m.cfg.Rules
}

// isZero reports whether a RuleSet carries no configuration at all, as opposed
// to a deliberately empty list. The distinction matters: a nil list means "the
// caller did not say", and a list that happens to be empty because every entry
// was removed means "crawl all of it".
func (r RuleSet) isZero() bool {
	return r.DisallowPathPrefixes == nil &&
		r.DisallowQueryParams == nil &&
		r.SkipFileExtensions == nil &&
		r.LangSubdomainDomains == nil &&
		r.WikiNoisyNamespaces == nil
}

// SkipsPath reports whether a path is one the crawler will not index.
//
// The comparison is on path *segments*, not on a raw string prefix. A plain
// HasPrefix meant "/cart" also swallowed "/cartoon" and "/cartoonography",
// "/search" swallowed "/searching" and "/search-archive", and "/login" swallowed
// "/logins-archive" -- all ordinary content pages that then never reached the
// index, with no error anywhere to say why.
func (r RuleSet) SkipsPath(p string) bool {
	cleaned := strings.Trim(p, "/")
	if cleaned == "" {
		return false
	}
	for _, prefix := range r.DisallowPathPrefixes {
		trimmed := strings.Trim(prefix, "/")
		if trimmed == "" {
			// An empty entry would otherwise match everything: after trimming,
			// `cleaned == ""` is false and `strings.HasPrefix(cleaned, "/")` is
			// false too, so the loop simply continues. Left explicit anyway,
			// because the alternative -- getting that reasoning wrong -- blocks
			// an entire host.
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

// SkipsExtension reports whether a path names a file type worth refusing before
// it is fetched.
//
// The path is lower-cased first. "/Report.PDF" and "/report.pdf" are the same
// document to every server that matters, and the old check compared against the
// raw path, so a capitalised extension was fetched and parsed before being found
// useless.
func (r RuleSet) SkipsExtension(p string) bool {
	lower := strings.ToLower(p)
	for _, ext := range r.SkipFileExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// DropsQueryParam reports whether a query key is dropped during
// canonicalisation.
func (r RuleSet) DropsQueryParam(q string) bool {
	for _, drop := range r.DisallowQueryParams {
		if q == drop {
			return true
		}
	}
	return false
}

// SkipsWikiLanguageSubpage reports whether a path is a translated copy of a
// page in a namespace that is noise in the first place.
//
// The two conditions are checked together deliberately. A language subpage
// anywhere on an ordinary article path is worth having -- the English mirror of
// a translated article is real content -- so matching the subpage alone would
// throw those away. It is specifically the combination with a noisy namespace
// that is being declined, because a translated "Template:Infobox" is neither
// indexed as an article nor useful as one.
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

// FoldsLanguageSubdomain rewrites a language-specific subdomain onto its English
// form, so a crawl of the wiki does not walk every translation of every site.
//
// It returns the host unchanged for a host that is not a known language domain,
// which is the common case and the reason the domain list exists at all: this
// rule must not fire on an arbitrary site whose subdomain happens to be two
// letters, because a genuine subdomain like "go" or "it" would be rewritten to
// a host that does not resolve.
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

// IsEnglish reports whether a document's declared language is one we index.
//
// An empty lang is English, not "unknown". The overwhelming majority of pages
// omit the attribute, and treating absence as a reason to decline would gut the
// index. A page that is actually in another language and does not say so is a
// problem no amount of attribute-reading can solve.
//
// The parser used to make this call itself and report the refusal as an error,
// which meant the crawl log showed "failed to fetch and parse page" for every
// non-English page ever seen -- indistinguishable from a network failure, and
// impossible to count. It now reports the attribute and this function decides,
// so a refusal is a policy outcome with a reason and lands in the stats hash.
func IsEnglish(lang string) bool {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		return true
	}

	// The primary subtag, not a prefix match. "en-US" and "en_US" are English;
	// "enochian" and "english" merely start with the letters, and a prefix
	// comparison indexes them. The old parser had this same looseness.
	primary := lang
	if i := strings.IndexAny(primary, "-_"); i >= 0 {
		primary = primary[:i]
	}

	// "en" is the BCP-47 tag; "eng" is the ISO 639-2 code that older markup
	// still emits, so both count.
	return primary == "en" || primary == "eng"
}

// --- robots.txt ---

// RobotsRules is a host's parsed robots.txt, compiled for matching.
//
// It is a value rather than a pointer so a host's rules can live in the host
// hash and be compared, and it is compiled once per host rather than per URL:
// the regex compilation is the expensive part and the old code did it inside the
// per-URL check, so every URL on a host recompiled every rule on that host.
type RobotsRules struct {
	// Allow and Disallow are the patterns, verbatim, in the order they appeared.
	// Order is kept for the log line a refusal produces; matching does not
	// depend on it.
	Allow    []string
	Disallow []string

	allow    []compiledRule
	disallow []compiledRule
}

// compiledRule pairs a pattern with the compiled form used to match it.
type compiledRule struct {
	// source is the rule as written, for the log line.
	source string
	// re is the compiled pattern, anchored at the start of the path.
	re *regexp.Regexp
	// weight is the rule's length, which is how the standard says specificity
	// is measured.
	weight int
}

// NewRobotsRules compiles a host's allow and disallow patterns.
//
// Uncompilable patterns are dropped rather than failing the call. A robots.txt
// is untrusted input written by whoever runs the site, and a rule this code
// cannot understand is a reason to not obey that rule, not a reason to stop
// crawling the host.
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

// compileRobotsRule turns one robots.txt pattern into a matcher.
//
// The pattern language is small and it is *not* a regular expression: `*` stands
// for any run of characters, a trailing `$` anchors the end, and every other
// character -- including `.`, `+`, `?`, `(` and `|` -- is literal.
//
// This is a behaviour change from the old matcher, and it goes in both
// directions, so it is worth being explicit about. The old one treated any rule
// containing a metacharacter as a regular expression, so `Disallow: /private.`
// blocked "/privateX" and, worse, a rule like `Disallow: (a|b)` blocked half the
// site. The standard's reading is that those are literal strings, so they match
// exactly themselves. The consequence is that some rules now block less than
// before -- a regex that never fired -- and some block more: "/private." now
// blocks the real "/private." file and stops blocking "/privateX".
//
// The important property is the direction of the failure. A pattern this code
// misreads as a broad regex is a rule that blocks more than the operator asked
// for, which for a crawler means silently declining a site. A pattern read too
// narrowly means fetching something that was nominally disallowed, which is
// impolite but recoverable and visible in the log.
func compileRobotsRule(rule string) (compiledRule, bool) {
	rule = strings.TrimSpace(rule)
	// Kept for the log line and for the weight. Both are about what the file said,
	// not about the form the matcher happens to compile it into -- otherwise the
	// same rule would be reported as two different things depending on whether it
	// ended in a slash.
	original := rule

	// An empty Disallow means "nothing is disallowed", which is the standard's
	// way of saying it and appears in real files. Appending it to the rule list
	// as a pattern would be a pattern matching everything.
	if rule == "" {
		return compiledRule{}, false
	}

	anchorEnd := strings.HasSuffix(rule, "$")
	if anchorEnd {
		rule = strings.TrimSuffix(rule, "$")
	}

	// A rule ending in "/" also matches the path without it. Canonicalisation
	// collapses "/wiki/" and "/wiki" onto one key because the crawler treats them
	// as one document, so the question asked of the rules is only ever about the
	// slash-less form. Matched strictly, "Allow: /wiki/" would fail to allow
	// "https://example.com/wiki" and the site would be blocked by the very
	// "Disallow: /" it paired the Allow with.
	//
	// The alternative is "/wiki(/|$)" rather than "/wiki?", because the latter also
	// matches "/wikipedia" -- an ordinary article page that the site never excluded.
	// "/" alone is left alone: stripping it would leave an empty rule, which is the
	// standard's "nothing is disallowed" and would stop "Disallow: /" from
	// excluding anything at all.
	optionalSlash := len(rule) > 1 && strings.HasSuffix(rule, "/")
	if optionalSlash {
		// Stripped, not appended to: keeping the slash and then making another one
		// optional compiles "/wiki/" to "^/wiki/(/|$)", which needs two slashes and
		// so matches neither "/wiki" nor "/wiki/Article" -- the rule that made the
		// site crawlable would exclude the entire site.
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

// Allows reports whether a path may be crawled under these rules, and which rule
// decided it.
//
// The standard's rule is longest-match-wins, with Allow beating Disallow at
// equal length. Both halves are load-bearing and the old matcher had neither,
// which is why a site writing the ordinary
//
//	Allow: /wiki/
//	Disallow: /
//
// -- "come to the good part" -- was blocked in full. The Disallow matched
// everything and the Allow that would have unblocked the interesting part was
// parsed and then discarded, so the crawler obeyed the blunter half of an
// instruction and lost the site.
func (r RobotsRules) Allows(path string) (bool, string) {
	if path == "" {
		// A host root is "/" and matching it as the empty string would find
		// neither a "/foo" rule nor the "/" rule that governs everything.
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
		// ">=" rather than ">", so an Allow of the same length as the best
		// Disallow replaces it. That is the tie-break the standard specifies.
		if !decided || rule.weight >= best.weight {
			best, allowed, decided = rule, true, true
		}
	}

	if !decided {
		return true, ""
	}
	return allowed, best.source
}

// Refuses reports whether a path is blocked, discarding which rule did it.
func (r RobotsRules) Refuses(path string) bool {
	allowed, _ := r.Allows(path)
	return !allowed
}
