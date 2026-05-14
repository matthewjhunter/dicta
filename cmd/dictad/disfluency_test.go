package main

import (
	"testing"
)

func TestStripDisfluencies_DefaultList(t *testing.T) {
	re := compileDisfluencyRE(defaultDisfluencies)
	if re == nil {
		t.Fatal("default list should compile to a non-nil regex")
	}
	cases := []struct {
		in, want string
	}{
		// Simple inline removal.
		{"I, uh, think we should go", "I, think we should go"},
		{"I, um, think we should go", "I, think we should go"},
		{"I want to, uh, write a function", "I want to, write a function"},

		// Leading filler.
		{"Uh, hello world", "hello world"},
		{"Um, I don't know", "I don't know"},
		{"Erm, maybe later", "maybe later"},

		// Trailing filler.
		{"hello uh.", "hello"},
		{"this is a test um", "this is a test"},

		// Repeated h's / m's (extended variants).
		{"well uhh I guess", "well I guess"},
		{"uhhh that is wrong", "that is wrong"},
		{"hmm let me think", "let me think"},
		{"hmmm okay", "okay"},

		// Case insensitivity.
		{"UH, I see", "I see"},
		{"Hello UM world", "Hello world"},

		// Multiple fillers in one utterance.
		{"Uh, um, well I think", "well I think"},
		{"I, uh, want, um, that", "I, want, that"},

		// Trailing ellipsis trim (separate from token strip).
		{"I think we should go...", "I think we should go"},
		{"Maybe later..", "Maybe later"},
		{"Done…", "Done"},
		{"Real sentence.", "Real sentence."},   // single dot survives
		{"etc. and so on.", "etc. and so on."}, // single dots survive

		// Pure-filler utterance collapses to empty.
		{"uh", ""},
		{"um.", ""},
		{"uh um", ""},
		{"Uh, um.", ""},

		// Filler-like-but-not in another word (boundary check).
		{"umbrella is open", "umbrella is open"},
		{"Burma is a country", "Burma is a country"},
		{"murmur softly", "murmur softly"},
		{"summer", "summer"},
		{"hummingbird", "hummingbird"},

		// No disfluency present.
		{"This is a normal sentence.", "This is a normal sentence."},
		{"", ""},
	}
	for _, tc := range cases {
		got := stripDisfluencies(tc.in, re)
		if got != tc.want {
			t.Errorf("stripDisfluencies(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripDisfluencies_NilRegexStillTrimsEllipsis(t *testing.T) {
	// With stripping disabled, trailing-ellipsis trim still runs —
	// it's a separate concern from token removal and is not gated by
	// the strip list.
	got := stripDisfluencies("hello world...", nil)
	if got != "hello world" {
		t.Errorf("trailing ellipsis should trim even when re is nil; got %q", got)
	}
	got = stripDisfluencies("uh, hello", nil)
	if got != "uh, hello" {
		t.Errorf("with nil re, token strip should not run; got %q", got)
	}
}

func TestCompileDisfluencyRE_EmptyInputReturnsNil(t *testing.T) {
	cases := []string{"", ",", " , , ", "   "}
	for _, in := range cases {
		if re := compileDisfluencyRE(in); re != nil {
			t.Errorf("compileDisfluencyRE(%q) should return nil; got %v", in, re)
		}
	}
}

func TestCompileDisfluencyRE_CustomList(t *testing.T) {
	// User adds "ah" and "you know" to the strip list. "ah" must NOT
	// match inside "Sarah". "you know" with internal space tests that
	// the word-boundary anchoring still works for multi-word entries.
	re := compileDisfluencyRE("ah,you know")
	if re == nil {
		t.Fatal("custom list should compile to non-nil")
	}
	cases := []struct {
		in, want string
	}{
		{"ah, that makes sense", "that makes sense"},
		{"I, you know, think so", "I, think so"},
		{"Sarah is here", "Sarah is here"}, // "ah" inside word: no match
		{"Hannah waved", "Hannah waved"},   // trailing "ah" inside word
		{"you know what I mean", "what I mean"},
	}
	for _, tc := range cases {
		got := stripDisfluencies(tc.in, re)
		if got != tc.want {
			t.Errorf("custom-list stripDisfluencies(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCompileDisfluencyRE_QuotesRegexMetaChars(t *testing.T) {
	// A user typo like "uh." should be treated as a literal token, not
	// a regex with "." as any-char. Confirm by stripping "uhx" doesn't
	// match.
	re := compileDisfluencyRE("uh.")
	if re == nil {
		t.Fatal("expected non-nil regex")
	}
	got := stripDisfluencies("uhx is fine", re)
	if got != "uhx is fine" {
		t.Errorf("regex metachars must be quoted; got %q", got)
	}
}
