package policy

import "strings"

// This file collects the rules that decide what a crawler is willing to fetch.
// The rest of them are relocated from utils in the centralisation phase; the
// language rule lands here first because the parser has to stop making the
// decision before the manager can own it.

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
