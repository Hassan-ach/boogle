package util

import (
	"math"
	"testing"
)

// setEnv uses t.Setenv, which fails the test if it is called from a parallel
// one and restores the previous value afterwards.
func TestGetWithDefaultReturnsTheValueWhenSet(t *testing.T) {
	t.Setenv("BOOGLE_TEST_STR", "configured")

	if got := GetWithDefault("BOOGLE_TEST_STR", "fallback"); got != "configured" {
		t.Errorf("GetWithDefault() = %q, want %q", got, "configured")
	}
}

func TestGetWithDefaultReturnsTheDefaultWhenUnset(t *testing.T) {
	if got := GetWithDefault("BOOGLE_TEST_STR_ABSENT", "fallback"); got != "fallback" {
		t.Errorf("GetWithDefault() = %q, want %q", got, "fallback")
	}
}

func TestGetWithDefaultTreatsAnEmptyValueAsUnset(t *testing.T) {
	// `PG_HOST=` in a compose file means "I forgot", not "the empty host".
	t.Setenv("BOOGLE_TEST_STR", "")

	if got := GetWithDefault("BOOGLE_TEST_STR", "fallback"); got != "fallback" {
		t.Errorf("GetWithDefault() = %q, want the default %q", got, "fallback")
	}
}

func TestGetWithDefaultDoesNotDistinguishAbsentFromEmpty(t *testing.T) {
	// Both mean the same thing to the caller, and the two must not drift apart.
	t.Setenv("BOOGLE_TEST_EMPTY", "")
	t.Setenv("BOOGLE_TEST_WS", " ")

	if GetWithDefault("BOOGLE_TEST_EMPTY", "d") != GetWithDefault("BOOGLE_TEST_ABSENT", "d") {
		t.Error("an empty value and an absent value must resolve the same way")
	}
	// Whitespace is not empty, so it is a legitimate value.
	if got := GetWithDefault("BOOGLE_TEST_WS", "d"); got != " " {
		t.Errorf("GetWithDefault() = %q, want the whitespace value to be kept", got)
	}
}

func TestGetIntWithDefault(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  int
	}{
		{"parses an integer", true, "42", 42},
		{"parses zero", true, "0", 0},
		{"parses a negative", true, "-7", -7},
		{"parses a leading plus", true, "+7", 7},
		// Atoi is base 10 on purpose. These are ports, counts and weights, and
		// accepting "0x10" or "0b11" for them would only hide a typo.
		{"falls back on a hex literal", true, "0x10", 99},
		{"falls back on an octal literal", true, "0o10", 99},
		{"falls back when unset", false, "", 99},
		{"falls back when empty", true, "", 99},
		{"falls back on a non-number", true, "abc", 99},
		{"falls back on a float", true, "4.2", 99},
		{"falls back on trailing junk", true, "42abc", 99},
		// strconv is strict, so a stray space is a misconfiguration rather than
		// something to paper over. Falling back to a known-good default beats
		// starting with a value nobody chose.
		{"falls back on surrounding space", true, "  42  ", 99},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("BOOGLE_TEST_INT", tc.value)
			}
			if got := GetIntWithDefault("BOOGLE_TEST_INT", 99); got != tc.want {
				t.Errorf("GetIntWithDefault(%q, 99) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestGetFloatWithDefault(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  float64
	}{
		{"parses a float", true, "4.2", 4.2},
		{"parses a whole number", true, "4", 4},
		{"parses a negative", true, "-0.5", -0.5},
		{"parses exponent notation", true, "1e3", 1000},
		// Go's float grammar accepts a trailing or leading dot, so these are
		// legitimate values rather than typos.
		{"parses a trailing dot", true, "4.", 4},
		{"parses a leading dot", true, ".5", 0.5},
		{"parses infinity", true, "Inf", math.Inf(1)},
		{"falls back when unset", false, "", 1.5},
		{"falls back when empty", true, "", 1.5},
		{"falls back on a non-number", true, "abc", 1.5},
		{"falls back on a bare dot", true, ".", 1.5},
		{"falls back on surrounding space", true, "  4.2  ", 1.5},
		// NaN is parseable but poisons every comparison it takes part in, so a
		// ranking weight that becomes NaN silently disables ranking.
		{"falls back on NaN", true, "NaN", 1.5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("BOOGLE_TEST_FLOAT", tc.value)
			}
			if got := GetFloatWithDefault("BOOGLE_TEST_FLOAT", 1.5); got != tc.want {
				t.Errorf("GetFloatWithDefault(%q, 1.5) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestGetIntAndGetFloatAgreeOnWholeNumbers(t *testing.T) {
	// The ranker reads the same variable through both helpers depending on the
	// field, so "0.85" must not mean 85 in one place and 0 in the other.
	t.Setenv("BOOGLE_TEST_BOTH", "2")

	if got := GetIntWithDefault("BOOGLE_TEST_BOTH", -1); got != 2 {
		t.Errorf("GetIntWithDefault() = %d, want 2", got)
	}
	if got := GetFloatWithDefault("BOOGLE_TEST_BOTH", -1); got != 2 {
		t.Errorf("GetFloatWithDefault() = %v, want 2", got)
	}
}

func TestDefaultsAreUsedIndependently(t *testing.T) {
	// One bad variable must not drag the others down with it.
	t.Setenv("BOOGLE_TEST_GOOD", "10")
	t.Setenv("BOOGLE_TEST_BAD", "not-a-number")

	if got := GetIntWithDefault("BOOGLE_TEST_GOOD", 1); got != 10 {
		t.Errorf("GetIntWithDefault(good) = %d, want 10", got)
	}
	if got := GetIntWithDefault("BOOGLE_TEST_BAD", 1); got != 1 {
		t.Errorf("GetIntWithDefault(bad) = %d, want the default 1", got)
	}
	if got := GetIntWithDefault("BOOGLE_TEST_ABSENT", 1); got != 1 {
		t.Errorf("GetIntWithDefault(absent) = %d, want the default 1", got)
	}
}
