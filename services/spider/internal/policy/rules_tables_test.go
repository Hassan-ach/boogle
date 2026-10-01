package policy

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

// TestSkipsPathMatchesWholeSegments is the reason the comparison is not a string
// prefix.
//
// "/cart" as a prefix swallows "/cartoon" and "/cartography"; "/search" swallows
// "/searching" and "/search-archive"; "/login" swallows "/logins-archive". All of
// those are ordinary content pages, and all of them then never reached the index
// with no error anywhere to say why.
func TestSkipsPathMatchesWholeSegments(t *testing.T) {
	r := DefaultRules()

	skipped := []string{
		"/login",
		"/login/",
		"/login/form",
		"/account/settings",
		"/cart/checkout",
	}
	for _, p := range skipped {
		if !r.SkipsPath(p) {
			t.Errorf("SkipsPath(%q) = false, want true", p)
		}
	}

	kept := []string{
		"/",
		"",
		// Paths are case-sensitive, and a rule written "/login" is not a rule about
		// "/LOGIN". A server that serves both serves two documents, and the
		// uppercase one is an ordinary page.
		"/LOGIN",
		"/cartoon",
		"/cartography",
		"/searching",
		"/search-archive",
		"/logins-archive",
		"/administration",
		"/administrators-guide",
		"/blog/post-about-login",
		"/articles/checkout-flow",
		"/errors",
	}
	for _, p := range kept {
		if r.SkipsPath(p) {
			t.Errorf("SkipsPath(%q) = true, want false", p)
		}
	}
}

// TestSkipsPathIgnoresEmptyEntries guards a table entry that would block a host.
//
// The comparison trims slashes off each rule, and a rule that trims to nothing
// would then be compared against the whole path. An empty string is a prefix of
// everything, so one blank line in a configuration file would make every URL on
// every host un-crawlable.
// TestRobotsRulesSpecificityUsesTheRuleAsWritten pins the one detail the
// longest-match rule is easy to get subtly wrong, and which no other case in this
// file distinguishes.
//
// A rule ending in "/" or "$" is compiled to a shorter pattern than it was
// written as -- "/a/" becomes "/a(/|$)" -- so measuring specificity on the
// compiled form makes the verdict depend on whether the site happened to write a
// trailing slash. Two rule sets that mean the same thing would be obeyed
// differently.
//
// The standard measures the pattern as the file wrote it. So in this pair the
// Disallow is three characters and the Allow is two, the Disallow is more
// specific, and the page is refused. Measured on the compiled form the two tie,
// the tie-break hands it to the Allow, and the page is crawled -- a silent
// difference from a cosmetic change to someone's robots.txt.
func TestRobotsRulesSpecificityUsesTheRuleAsWritten(t *testing.T) {
	r := NewRobotsRules(
		[]string{"/a"},
		[]string{"/a/"},
	)

	for _, path := range []string{"/a", "/a/"} {
		allowed, rule := r.Allows(path)
		if allowed {
			t.Errorf("Allows(%q) = true (decided by %q), want false: the longer "+
				"rule as written is the disallow", path, rule)
		}
		if rule != "/a/" {
			t.Errorf("Allows(%q) was decided by %q, want the rule as written, %q",
				path, rule, "/a/")
		}
	}

	// The same two rules with the slash on the Allow instead: now the Allow is the
	// longer one, so it wins on length rather than on the tie-break. Both halves
	// are needed, or a test would pass with the weight computed any old way.
	r = NewRobotsRules([]string{"/a/"}, []string{"/a"})
	if allowed, rule := r.Allows("/a"); !allowed {
		t.Errorf("Allows(\"/a\") = false (decided by %q), want true: the longer "+
			"rule as written is now the allow", rule)
	}
}

func TestSkipsPathIgnoresEmptyEntries(t *testing.T) {
	r := RuleSet{DisallowPathPrefixes: []string{"", "/", "//", "  "}}

	for _, p := range []string{"/", "/a", "/a/b/c", "/article"} {
		if r.SkipsPath(p) {
			t.Errorf("SkipsPath(%q) = true with an empty rule in the table", p)
		}
	}
}

// TestSkipsExtensionIsCaseInsensitive covers the check the old one got wrong.
//
// "/Report.PDF" and "/report.pdf" are the same document to every server that
// matters, and comparing against the raw path meant a capitalised extension was
// transferred in full and then parsed as HTML.
func TestSkipsExtensionIsCaseInsensitive(t *testing.T) {
	r := DefaultRules()

	for _, p := range []string{
		"/report.pdf", "/Report.PDF", "/REPORT.Pdf",
		"/deep/path/file.DOCX",
		"/img.PNG",
		"/static/app.JS",
	} {
		if !r.SkipsExtension(p) {
			t.Errorf("SkipsExtension(%q) = false, want true", p)
		}
	}

	for _, p := range []string{
		"/article",
		"/v1.2/article",    // a dot in a directory is not an extension
		"/report.pdf/page", // a trailing slash makes it a directory
		"/pdf",             // no dot
		"/a.jsp?x=1",       // query is not part of the path
		"/style.css.map",   // only the final extension counts
	} {
		if r.SkipsExtension(p) {
			t.Errorf("SkipsExtension(%q) = true, want false", p)
		}
	}
}

// TestSkipsWikiLanguageSubpageRequiresBothConditions is a judgement call written
// down.
//
// A language subpage on an ordinary article is worth having -- the English mirror
// of a translated article is real content, and it is often the only copy that
// exists. What is being declined is a *translated template*: neither indexed as an
// article nor useful as one, and the largest single source of low-value pages in a
// wiki crawl.
func TestSkipsWikiLanguageSubpageRequiresBothConditions(t *testing.T) {
	r := DefaultRules()

	skipped := []string{
		"/wiki/Template:Infobox/fr/",
		"/wiki/Template:Infobox/fr",
		"/wiki/Help:Contents/de",
		"/wiki/Manual:Style/en",
		"/wiki/Extension:Something/zh-yue",
		// The noisy namespace does not have to be the last segment.
		"/wiki/Template:Infobox/doc/fr/sub",
	}
	for _, p := range skipped {
		if !r.SkipsWikiLanguageSubpage(p) {
			t.Errorf("SkipsWikiLanguageSubpage(%q) = false, want true", p)
		}
	}

	kept := []string{
		// Real content.
		"/wiki/Paris/fr",
		"/wiki/Paris/fr/",
		"/wiki/Template:Infobox",     // a noisy namespace in English is still noise, but not a translated copy
		"/wiki/Help:Contents",        // same
		"/articles/how-to-choose-fr", // the trailing token is not a language subpage of a noisy namespace
		"/wiki/Template:Infobox/format",
	}
	for _, p := range kept {
		if r.SkipsWikiLanguageSubpage(p) {
			t.Errorf("SkipsWikiLanguageSubpage(%q) = true, want false", p)
		}
	}
}

// TestFoldsLanguageSubdomainOnlyOnKnownDomains is the guard that stops this rule
// doing damage.
//
// Folding is a rewrite of a hostname, so applying it to an arbitrary site whose
// subdomain happens to be two letters sends the crawl to a host that does not
// resolve -- "go.example.com" becomes "en.example.com", which is some other
// company's intranet.
func TestFoldsLanguageSubdomainOnlyOnKnownDomains(t *testing.T) {
	r := DefaultRules()

	cases := []struct{ in, want string }{
		{"fr.wikipedia.org", "en.wikipedia.org"},
		{"de.wikibooks.org", "en.wikibooks.org"},
		{"zh-min-nan.wikivoyage.org", "en.wikivoyage.org"},
		{"en.wikipedia.org", "en.wikipedia.org"},
		{"wikipedia.org", "wikipedia.org"},
		{"www.fr.wikipedia.org", "en.wikipedia.org"},

		// Not a language domain: untouched, whatever the subdomain looks like.
		{"go.example.com", "go.example.com"},
		{"it.example.co.uk", "it.example.co.uk"},
		{"FR.EXAMPLE.COM", "fr.example.com"},
		{"blog.example.com", "blog.example.com"},
	}

	for _, tc := range cases {
		if got := r.FoldsLanguageSubdomain(tc.in); got != tc.want {
			t.Errorf("FoldsLanguageSubdomain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRuleSetZeroMeansDefaultRules closes the trap in a nil rule table.
//
// A RuleSet built as a struct literal has nil lists, and nil means "the caller did
// not say anything". Reading that as "no rules" turns every restricted path, every
// binary file and every translated template into something the crawler will
// happily fetch -- the exact class of bug that turns a strict crawler into an
// impolite one, reached by forgetting a field.
func TestRuleSetZeroMeansDefaultRules(t *testing.T) {
	m, _ := newTestManager(t)

	if got := m.rules(); got.isZero() {
		t.Fatal("the manager's default rules read as empty")
	}

	var zero Config
	m2 := New(zero, NewMemoryState(), testLogger())
	if r := m2.rules(); r.isZero() {
		t.Error("a zero Config produced no rules; every skip rule would be disabled")
	} else if !r.SkipsPath("/login") {
		t.Error("a zero Config lost the shipped path rules")
	}

	// A deliberately emptied list is a decision, not an oversight, and must be
	// honoured.
	var permissive Config
	permissive.Rules = RuleSet{
		DisallowPathPrefixes: []string{},
		SkipFileExtensions:   []string{},
		WikiNoisyNamespaces:  []string{},
	}
	m3 := New(permissive, NewMemoryState(), testLogger())
	if m3.rules().SkipsPath("/login") {
		t.Error("an explicitly emptied rule list was replaced by the defaults")
	}
}

// TestRobotsRulesPreferTheLongestMatch covers the whole specificity rule, because
// it is the one piece of the robots standard most crawlers get wrong.
//
// Two halves, and both are load-bearing. Without "longest wins", whichever rule is
// checked first decides, so a nested carve-out either overrides the broad rule or
// is overridden by it. Without "Allow wins ties", the ordinary
//
//	Allow: /wiki/
//	Disallow: /
//
// would be decided by whichever of the two is listed second.
func TestRobotsRulesPreferTheLongestMatch(t *testing.T) {
	// "/ab*" and "/abc" are both four characters and both match "/abc", which is the
	// only way to construct a genuine tie out of two patterns of different shapes --
	// and the tie is the case the standard calls out explicitly.
	r := NewRobotsRules(
		[]string{"/wiki/", "/public", "/ab*"},
		[]string{"/", "/wiki/private/", "/public/secret", "/abc"},
	)

	cases := []struct {
		path      string
		wantAllow bool
		rule      string
	}{
		{"/wiki/Article", true, "/wiki/"},
		{"/public/page", true, "/public"},
		{"/wiki/private/Notes", false, "/wiki/private/"},
		{"/public/secret/x", false, "/public/secret"},
		{"/other", false, "/"},
		// Same length, one Allow and one Disallow: the Allow wins.
		{"/abc", true, "/ab*"},
		// The host root. Matched as "/" rather than "", or neither a "/foo" rule
		// nor the "/" rule that governs everything would be found.
		{"/", false, "/"},
	}

	for _, tc := range cases {
		allowed, rule := r.Allows(tc.path)
		if allowed != tc.wantAllow {
			t.Errorf("Allows(%q) = %v (decided by %q), want %v", tc.path, allowed, rule, tc.wantAllow)
			continue
		}
		if rule != tc.rule {
			t.Errorf("Allows(%q) decided by %q, want %q", tc.path, rule, tc.rule)
		}
	}
}

// TestRobotsRulesTreatsABlankDisallowAsNothing is a bug this codebase had.
//
// The old parser appended the empty string for a bare "Disallow:", and
// strings.HasPrefix(p, "") is true for every p -- so a single blank directive in
// one robots.txt matched every path on the host and the crawler obeyed it. It
// survived review because the rule list looked correct and the comparison was
// three lines away.
func TestRobotsRulesTreatsABlankDisallowAsNothing(t *testing.T) {
	r := NewRobotsRules(nil, []string{"/private/", ""})

	if r.Refuses("/anything") {
		t.Error("a blank Disallow rule blocked a path")
	}
	if !r.Refuses("/private/x") {
		t.Error("a real Disallow rule was ignored")
	}

	// And the same at the parse level, which is where the empty string entered.
	parsed := parseRobots("User-agent: *\nDisallow:\nDisallow: /x\n", "*")
	if len(parsed.Disallow) != 1 || parsed.Disallow[0] != "/x" {
		t.Errorf("Disallow = %#v, want [\"/x\"]", parsed.Disallow)
	}
}

// TestRobotsRulesArePatternsNotRegularExpressions documents a deliberate behaviour
// change, in both directions.
//
// The old matcher treated any rule containing a metacharacter as a regular
// expression. So "Disallow: /private." blocked "/privateX" and never the actual
// "/private." file, and a rule like "Disallow: (a|b)" blocked half the site. The
// standard's pattern language is much smaller -- `*` for any run, a trailing `$`
// to anchor, everything else literal -- and a rule this code cannot read as a
// pattern is a rule read too narrowly, which errs towards fetching something
// nominally disallowed rather than silently declining a site.
func TestRobotsRulesArePatternsNotRegularExpressions(t *testing.T) {
	t.Run("metacharacters are literal", func(t *testing.T) {
		r := NewRobotsRules(nil, []string{`/private.`, `/a+b`, `/x(y|z)`})

		if !r.Refuses("/private.") {
			t.Error("a literal dot did not match itself")
		}
		if r.Refuses("/privateX") {
			t.Error("a dot was treated as a regex wildcard")
		}
		if r.Refuses("/a+b") != true {
			t.Error("'a+b' did not match itself")
		}
		if r.Refuses("/aab") {
			t.Error("'+' was treated as a quantifier")
		}
		if r.Refuses("/x(y|z)") != true {
			t.Error("a group did not match itself")
		}
		if r.Refuses("/xy") {
			t.Error("an alternation was interpreted")
		}
	})

	t.Run("star is a wildcard", func(t *testing.T) {
		r := NewRobotsRules(nil, []string{"/a/*/c"})

		if !r.Refuses("/a/b/c") {
			t.Error("'*' did not match a single segment")
		}
		if !r.Refuses("/a/anything/deeply/c") {
			t.Error("'*' did not match a run of segments")
		}
		if r.Refuses("/a/b/d") {
			t.Error("'*' matched the wrong tail")
		}
	})

	t.Run("trailing dollar anchors", func(t *testing.T) {
		r := NewRobotsRules(nil, []string{"/exact$"})

		if !r.Refuses("/exact") {
			t.Error("an anchored rule did not match")
		}
		if r.Refuses("/exact/more") {
			t.Error("'$' did not anchor the end")
		}
	})

	t.Run("a trailing slash also matches without it", func(t *testing.T) {
		r := NewRobotsRules([]string{"/wiki/"}, []string{"/"})

		// Canonicalisation collapses "/wiki/" and "/wiki" onto one key, so the rule
		// has to cover the form the crawler actually asks about.
		for _, p := range []string{"/wiki", "/wiki/", "/wiki/Article"} {
			if r.Refuses(p) {
				t.Errorf("'Allow: /wiki/' did not match %s", p)
			}
		}
		// But not the near-miss that a naive "optional slash" would have caught.
		if !r.Refuses("/wikipedia") {
			t.Error("'Allow: /wiki/' matched /wikipedia")
		}
	})

	t.Run("a bare slash still excludes everything", func(t *testing.T) {
		r := NewRobotsRules(nil, []string{"/"})

		for _, p := range []string{"/", "/anything", "/deep/path"} {
			if !r.Refuses(p) {
				t.Errorf("'Disallow: /' did not exclude %s", p)
			}
		}
	})
}

// TestRobotsRulesCompileOnce checks that the matcher is a value rather than
// something rebuilt per URL.
//
// Compilation is the expensive part and the old code did it inside the per-URL
// check, so every URL on a host recompiled every rule on that host. A host with
// three hundred rules paid three hundred compilations per page.
func TestRobotsRulesCompileOnce(t *testing.T) {
	r := NewRobotsRules([]string{"/a", "/b"}, []string{"/c", "/d"})
	if len(r.allow) != 2 || len(r.disallow) != 2 {
		t.Fatalf("compiled %d allow and %d disallow rules, want 2 and 2", len(r.allow), len(r.disallow))
	}
	for _, c := range append(append([]compiledRule{}, r.allow...), r.disallow...) {
		if c.re == nil {
			t.Errorf("rule %q compiled to nil", c.source)
		}
		if c.weight <= 0 {
			t.Errorf("rule %q has weight %d; specificity cannot be measured", c.source, c.weight)
		}
	}
}

// TestParseRobots covers the parser against the awkward shapes real files have.
func TestParseRobots(t *testing.T) {
	const body = `
# a comment on its own line
User-agent: *
Disallow: /private/
Disallow: /private/   # a trailing comment on a directive
Allow: /private/public/
Crawl-delay: 5

User-agent: BoogleBot
Disallow: /boogle-only/

Sitemap: https://example.com/sitemap.xml
Sitemap: https://example.com/news.xml
Sitemap: https://example.com/sitemap.xml
`

	got := parseRobots(body, "BoogleBot")

	wantDisallow := []string{"/private/", "/boogle-only/"}
	if !equalStrings(got.Disallow, wantDisallow) {
		t.Errorf("Disallow = %#v, want %#v", got.Disallow, wantDisallow)
	}
	if want := []string{"/private/public/"}; !equalStrings(got.Allow, want) {
		t.Errorf("Allow = %#v, want %#v", got.Allow, want)
	}
	if want := []string{"https://example.com/sitemap.xml", "https://example.com/news.xml"}; !equalStrings(got.SiteMaps, want) {
		t.Errorf("SiteMaps = %#v, want %#v", got.SiteMaps, want)
	}
	if got.CrawlDelay != 5 {
		t.Errorf("CrawlDelay = %d, want 5", got.CrawlDelay)
	}
}

// TestParseRobotsMergesEveryMatchingGroup covers two things the old parser got
// wrong, both about which stanza a rule belongs to.
func TestParseRobotsMergesEveryMatchingGroup(t *testing.T) {
	t.Run("consecutive agents share the rules below them", func(t *testing.T) {
		// This is the shape Google's own documentation recommends. Overwriting the
		// agent as each line was read gave the rules to the last one named, so a
		// crawler matching either agent got nothing.
		got := parseRobots("User-agent: *\nUser-agent: BoogleBot\nDisallow: /x\n", "BoogleBot")
		if !equalStrings(got.Disallow, []string{"/x"}) {
			t.Errorf("Disallow = %#v, want [\"/x\"]", got.Disallow)
		}
	})

	t.Run("a named agent we do not match contributes nothing", func(t *testing.T) {
		got := parseRobots("User-agent: googlebot\nDisallow: /\n\nUser-agent: *\nDisallow: /x\n", "BoogleBot")
		if equalStrings(got.Disallow, []string{"/"}) {
			t.Error("another crawler's rules were applied to us")
		}
		if !equalStrings(got.Disallow, []string{"/x"}) {
			t.Errorf("Disallow = %#v, want [\"/x\"]", got.Disallow)
		}
	})

	t.Run("a shared stanza applies through whichever agent we match", func(t *testing.T) {
		// The distinguishing case for a run of User-agent lines: the rule set is
		// shared, but only one of the named agents is us. If the stanza's rules were
		// given to the last agent named rather than to all of them, they would land
		// on "googlebot", never match, and the host would be crawled with no rules
		// at all -- the most consequential way this could go wrong, because a site
		// that writes this shape is stating its rules to us explicitly.
		got := parseRobots("User-agent: *\nUser-agent: googlebot\nDisallow: /\n", "BoogleBot")
		if !equalStrings(got.Disallow, []string{"/"}) {
			t.Errorf("Disallow = %#v, want [\"/\"]: a stanza shared with another "+
				"crawler has to reach us through the wildcard", got.Disallow)
		}

		// And the mirror image: we match the last agent named, not the first, and
		// the wildcard in a shared stanza still applies alongside it.
		got = parseRobots("User-agent: googlebot\nUser-agent: *\nDisallow: /a\n", "BoogleBot")
		if !equalStrings(got.Disallow, []string{"/a"}) {
			t.Errorf("Disallow = %#v, want [\"/a\"]", got.Disallow)
		}
	})

	t.Run("a wildcard group applies to everyone", func(t *testing.T) {
		got := parseRobots("User-agent: *\nDisallow: /x\n", "SomeOtherBot/2.0")
		if !equalStrings(got.Disallow, []string{"/x"}) {
			t.Errorf("Disallow = %#v, want [\"/x\"]", got.Disallow)
		}
	})

	t.Run("sitemaps belong to no agent", func(t *testing.T) {
		// The old parser had Sitemap in the same switch as the per-agent rules, so
		// it worked only by accident of where the line appeared.
		got := parseRobots("Sitemap: https://example.com/s.xml\nUser-agent: googlebot\n", "BoogleBot")
		if !equalStrings(got.SiteMaps, []string{"https://example.com/s.xml"}) {
			t.Errorf("SiteMaps = %#v, want the sitemap before any agent line", got.SiteMaps)
		}
	})
}

// TestParseRobotsIgnoresRulesBeforeAnyAgent covers the malformed case, where the
// choice is between two guesses.
func TestParseRobotsIgnoresRulesBeforeAnyAgent(t *testing.T) {
	got := parseRobots("Disallow: /orphan\nUser-agent: *\nDisallow: /real\n", "*")
	if equalStrings(got.Disallow, []string{"/orphan", "/real"}) {
		t.Error("a rule belonging to no agent was applied")
	}
	if !equalStrings(got.Disallow, []string{"/real"}) {
		t.Errorf("Disallow = %#v, want [\"/real\"]", got.Disallow)
	}
}

// TestParseRobotsTakesTheLargestCrawlDelay is the one place "largest" is right.
//
// Two groups both match us and both ask for a delay. The larger is the one the
// site would prefer us to honour, and honouring the smaller would mean ignoring an
// instruction that was addressed to us.
func TestParseRobotsTakesTheLargestCrawlDelay(t *testing.T) {
	got := parseRobots("User-agent: *\nCrawl-delay: 2\n\nUser-agent: BoogleBot\nCrawl-delay: 9\n", "BoogleBot")
	if got.CrawlDelay != 9 {
		t.Errorf("CrawlDelay = %d, want 9", got.CrawlDelay)
	}
}

// TestParseRobotsDedupes keeps the host hash from filling with forty copies of one
// pattern.
func TestParseRobotsDedupes(t *testing.T) {
	got := parseRobots(
		"User-agent: *\nDisallow: /x\nSitemap: https://e/s.xml\n\n"+
			"User-agent: BoogleBot\nDisallow: /x\nSitemap: https://e/s.xml\n",
		"BoogleBot")
	if len(got.Disallow) != 1 {
		t.Errorf("Disallow = %#v, want one entry", got.Disallow)
	}
	if len(got.SiteMaps) != 1 {
		t.Errorf("SiteMaps = %#v, want one entry", got.SiteMaps)
	}
}

// TestParseRobotsOnEmptyInput covers the input most hosts provide.
func TestParseRobotsOnEmptyInput(t *testing.T) {
	for _, body := range []string{"", "\n\n\n", "# nothing but comments\n"} {
		got := parseRobots(body, "BoogleBot")
		if got == nil {
			t.Fatalf("parseRobots(%q) = nil", body)
		}
		if len(got.Allow) != 0 || len(got.Disallow) != 0 || len(got.SiteMaps) != 0 {
			t.Errorf("parseRobots(%q) = %#v, want empty", body, got)
		}
		// An empty robots.txt permits everything, which is the difference between
		// "no rules" and "no access".
		if NewRobotsRules(got.Allow, got.Disallow).Refuses("/x") {
			t.Errorf("parseRobots(%q) refused a path; no rules means no restriction", body)
		}
	}
}

// TestParseRobotsKeepsEveryCaseOfADirective checks that a directive is recognised
// whatever case it is written in, because the standard says the keys are
// case-insensitive and real files use all of them.
func TestParseRobotsKeepsEveryCaseOfADirective(t *testing.T) {
	got := parseRobots("USER-AGENT: *\nDISALLOW: /x\nAllow: /y\ncRaWl-DeLaY: 3\nSITEMAP: https://e/s.xml\n", "*")

	if !equalStrings(got.Disallow, []string{"/x"}) {
		t.Errorf("Disallow = %#v, want [\"/x\"]", got.Disallow)
	}
	if !equalStrings(got.Allow, []string{"/y"}) {
		t.Errorf("Allow = %#v, want [\"/y\"]", got.Allow)
	}
	if got.CrawlDelay != 3 {
		t.Errorf("CrawlDelay = %d, want 3", got.CrawlDelay)
	}
	if !equalStrings(got.SiteMaps, []string{"https://e/s.xml"}) {
		t.Errorf("SiteMaps = %#v, want the sitemap", got.SiteMaps)
	}
}

// TestSplitDirective covers the split itself, including the case that decides it.
//
// The first colon is the separator, which is the only way a Sitemap value survives:
// "Sitemap: https://example.com/s.xml" splits into the key and the whole URL, and
// splitting on the last colon instead would leave "https://example" as the value.
func TestSplitDirective(t *testing.T) {
	cases := []struct {
		line, key, value string
		ok               bool
	}{
		{"Disallow: /x", "disallow", "/x", true},
		{"  Allow :   /y  ", "allow", "/y", true},
		{"Sitemap: https://e/s.xml", "sitemap", "https://e/s.xml", true},
		{"Disallow:/nospace", "disallow", "/nospace", true},
		{"Disallow: /a:b", "disallow", "/a:b", true},
		{"no colon here", "", "", false},
		{": leading colon", "", "", false},
	}

	for _, tc := range cases {
		key, value, ok := splitDirective(tc.line)
		if ok != tc.ok || key != tc.key || value != tc.value {
			t.Errorf("splitDirective(%q) = %q/%q/%v, want %q/%q/%v",
				tc.line, key, value, ok, tc.key, tc.value, tc.ok)
		}
	}
}

// TestTheRuleTablesAreTheOnlyCopy is the test that replaced the one that compared
// these rules against utils.NormalizeUrl.
//
// That comparison existed because there were two copies: a path table, an extension
// table and a wiki-namespace table here, and the same three in utils, applied inside
// canonicalisation. The utils copies are gone -- a URL that used to vanish at
// extraction time now arrives at Admit and is refused there with a reason -- and
// with them goes the reason to keep checking that they agree.
//
// What replaces it pins the move rather than the agreement. Every path the rule
// tables refuse must be refused by Admit, so the rules that used to hide in
// canonicalisation are still being applied by the layer that can count them. A
// table entry dropped during the move, or a reason that stopped being produced, is
// silent otherwise: the URL reaches the frontier, the crawler fetches it, and
// nothing in the logs says it should not have been.
func TestTheRuleTablesAreTheOnlyCopy(t *testing.T) {
	m, _ := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply:  map[string]robotsResponse{"example.com": {body: ""}},
		status: 200,
	}).fetcher())
	r := DefaultRules()

	for _, path := range ruleCorpus(r) {
		// The URL has to survive canonicalisation, or this test is asserting that a
		// rule fired when in fact the URL was rejected before any rule was reached.
		full := "https://example.com" + path
		if path == "" {
			full = "https://example.com/"
		}
		canon, ok := utils.CanonicalizeUrl(full, "example.com")
		if !ok {
			t.Errorf("CanonicalizeUrl(%q) rejected the URL, so this case is testing "+
				"canonicalisation rather than the rule tables", full)
			continue
		}
		u, err := url.Parse(canon)
		if err != nil {
			t.Fatalf("parse %q: %v", canon, err)
		}

		wantRefused := r.SkipsPath(u.Path) ||
			r.SkipsExtension(u.Path) ||
			r.SkipsWikiLanguageSubpage(u.Path)
		v := mustAdmit(t, m, canon)
		if wantRefused && v.Kind == Allow {
			t.Errorf("path %q: the rule tables refuse it but Admit allows it", path)
		}
		if !wantRefused && v.Kind != Allow {
			t.Errorf("path %q: Admit refused it for %q, but no rule covers it",
				path, v.Reason)
		}
	}
}

// TestEveryRefusedPathCarriesAReason is what "counted" has to mean.
//
// Admit can refuse a URL for nine or ten reasons, and a refusal whose reason is not
// one of them would be counted under the empty string -- a key no operator ever
// queries. So for each path the tables refuse, the verdict has to name a reason,
// and that reason has to be one of the rule reasons rather than something incidental
// to the fixture.
func TestEveryRefusedPathCarriesAReason(t *testing.T) {
	m, st := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply:  map[string]robotsResponse{"example.com": {body: ""}},
		status: 200,
	}).fetcher())
	r := DefaultRules()

	ruleReasons := map[Reason]bool{
		ReasonPathDisallowed:     true,
		ReasonExtensionSkipped:   true,
		ReasonLanguageNotEnglish: true,
	}

	for _, path := range ruleCorpus(r) {
		full := "https://example.com" + path
		if path == "" {
			full = "https://example.com/"
		}
		canon, ok := utils.CanonicalizeUrl(full, "example.com")
		if !ok {
			continue
		}
		u, err := url.Parse(canon)
		if err != nil {
			t.Fatalf("parse %q: %v", canon, err)
		}
		if !r.SkipsPath(u.Path) && !r.SkipsExtension(u.Path) && !r.SkipsWikiLanguageSubpage(u.Path) {
			continue
		}

		v := mustAdmit(t, m, canon)
		if v.Reason == "" {
			t.Errorf("path %q was refused with no reason, so it is counted under "+
				"the empty string", path)
			continue
		}
		if !ruleReasons[v.Reason] {
			t.Errorf("path %q was refused for %q, want one of the rule reasons %v",
				path, v.Reason, ruleReasons)
		}
	}

	// And the refusals really are counted, which is the whole reason they moved out
	// of canonicalisation: a rule applied there could not be observed at all.
	for _, reason := range []Reason{ReasonPathDisallowed, ReasonExtensionSkipped} {
		if got := st.Stats("example.com")[reason]; got == 0 {
			t.Errorf("no refusal counted under %q after admitting the corpus", reason)
		}
	}
}

// ruleCorpus builds the paths the rule-table tests run over.
//
// The shape of it is the point. A corpus of URLs I thought of would test the
// cases I already know about; this one is built from the rule tables themselves
// and from their near misses -- the cases where two implementations of the same
// rule actually disagree. The near misses matter more than the entries: a rule
// that matches too much is the failure mode nobody reports, because the pages it
// eats look like pages that simply never existed.
func ruleCorpus(r RuleSet) []string {
	seen := map[string]struct{}{}
	out := []string{}

	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}

	// Suffixes that turn a rule into a near miss. "/cart" plus "oon" is the bug
	// that motivated matching on segments; ".pdf" plus "/x" is a directory rather
	// than a file; "/fr" plus "ance" is an article rather than a language subpage.
	for _, suffix := range []string{"", "/", "/x", "/x/y", "oon", "oon/x", "-archive", "s", "/page"} {
		for _, entry := range r.DisallowPathPrefixes {
			add(entry + suffix)
			add(entry + strings.ToUpper(suffix))
		}
		for _, entry := range r.SkipFileExtensions {
			add("/dir" + entry + suffix)
			add("/dir" + strings.ToUpper(entry) + suffix)
		}
		for _, ns := range r.WikiNoisyNamespaces {
			for _, lang := range []string{"/fr", "/fr/", "/de", "/zh-yue", "/fr/sub", "", "/france"} {
				add("/wiki" + ns + "Infobox" + lang)
			}
		}
	}

	// Ordinary paths with no rule in them at all, which must all be kept.
	for _, p := range []string{
		"/", "", "/article", "/blog/2026/a-post", "/wiki/Paris",
		"/docs/getting-started", "/v1.2/api", "/user/settings-not-ours",
	} {
		add(p)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
