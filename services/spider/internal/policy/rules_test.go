package policy

import "testing"

func TestIsEnglish(t *testing.T) {
	tests := []struct {
		lang string
		want bool
	}{
		// An absent attribute is "no claim made", not "not English". Most real
		// pages omit lang, and treating absence as a refusal would gut the index.
		{"", true},
		{"   ", true},

		{"en", true},
		{"EN", true},
		{"en-US", true},
		{"en-GB", true},
		{"en_US", true},
		{"en-US-oxendict", true},
		{"  en-GB  ", true},
		// ISO 639-2 code, still emitted by older markup.
		{"eng", true},

		{"fr", false},
		{"de", false},
		{"es", false},
		{"ar", false},
		{"ru", false},
		{"zh-Hans", false},
		{"ja", false},
		// Case must not rescue a non-English tag.
		{"FR", false},
		{"DE", false},
		// And a word that merely starts with "en" is not the English tag.
		{"enochian", false},
		{"english", false},
	}

	for _, tc := range tests {
		if got := IsEnglish(tc.lang); got != tc.want {
			t.Errorf("IsEnglish(%q) = %v, want %v", tc.lang, got, tc.want)
		}
	}
}

// TestIsEnglishCoversWhatTheParserFound documents the coupling between the
// parser's reporting and this decision. The parser used to make the call itself
// and return an error, so a non-English page logged as a fetch failure and
// nothing counted it; now it reports and this judges.
func TestIsEnglishCoversWhatTheParserFound(t *testing.T) {
	cases := map[string]bool{
		"de":         false,
		"de-DE":      false,
		"pt-BR":      false,
		"en":         true,
		"en-AU":      true,
		"":           true,
		"nl-NL":      false,
		"fr-CA":      false,
		"es-419":     false,
		"zh-Hant-TW": false,
	}
	for lang, want := range cases {
		if got := IsEnglish(lang); got != want {
			t.Errorf("IsEnglish(%q) = %v, want %v", lang, got, want)
		}
	}
}
