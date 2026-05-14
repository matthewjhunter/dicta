package main

import (
	"regexp"
	"strings"
)

// defaultDisfluencies is the strip-list that ships with dictad. Each
// entry is a literal token; matching is case-insensitive and
// word-boundary anchored. Variants ("uh", "uhh", "uhhh") are listed
// explicitly so a user-supplied override is straightforward — no
// regex syntax leaks into the flag value.
//
// Real words that double as fillers in conversation ("ah", "well",
// "like", "you know") are intentionally NOT included: stripping them
// from dictated prose is more likely to delete intended content than
// remove an artifact. Add to the flag at your own risk.
const defaultDisfluencies = "uh,uhh,uhhh,um,umm,ummm,uhm,er,erm,hmm,hmmm"

var (
	repeatedCommaRE    = regexp.MustCompile(`,\s*,`)
	multiSpaceRE       = regexp.MustCompile(`[ \t]+`)
	trailingEllipsisRE = regexp.MustCompile(`\s*(?:\.{2,}|…+)\s*$`)
)

// compileDisfluencyRE turns a comma-separated list of literal tokens
// into a single anchored regex of the form
// `(?i)\b(tok1|tok2|...)\b[,.]?\s*` — case-insensitive, word-boundary
// anchored, consuming an optional trailing comma or period and any
// whitespace that followed.
//
// An empty list (or one that contains only empty entries) returns
// nil, which stripDisfluencies treats as "stripping disabled" so the
// daemon can opt out of word-level scrubbing entirely.
func compileDisfluencyRE(csv string) *regexp.Regexp {
	parts := strings.Split(csv, ",")
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		quoted = append(quoted, regexp.QuoteMeta(p))
	}
	if len(quoted) == 0 {
		return nil
	}
	pattern := `(?i)\b(` + strings.Join(quoted, "|") + `)\b[,.]?\s*`
	return regexp.MustCompile(pattern)
}

// stripDisfluencies removes filler tokens from text and tidies up the
// punctuation/whitespace left behind. Trailing ellipsis runs ("..",
// "...", "…") are also trimmed — Whisper emits them on lingering
// silence at end of utterance.
//
// If re is nil the function returns text unchanged; that's the
// "stripping disabled" path.
func stripDisfluencies(text string, re *regexp.Regexp) string {
	if re != nil {
		text = re.ReplaceAllString(text, "")
		text = repeatedCommaRE.ReplaceAllString(text, ",")
		text = multiSpaceRE.ReplaceAllString(text, " ")
	}
	text = trailingEllipsisRE.ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	// After stripping a leading filler ("Uh, hello" → ", hello"), the
	// orphan comma/space is meaningless. Strip leading punctuation
	// that could only have arrived this way.
	text = strings.TrimLeft(text, ",; ")
	return text
}
