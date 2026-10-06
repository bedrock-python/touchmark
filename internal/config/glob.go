package config

import (
	"fmt"
	"strings"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// Glob patterns over repository paths: the entries of exclude and the
// patterns of an org or group entry's match.
//
// A pattern is a repository path whose segments may hold glob characters:
// "*" matches any run of characters within one segment, "?" any one
// character, and a segment that is "**" spans any number of segments,
// none included. "**" inside a longer segment is "*" twice. Patterns
// compare ignoring case, as repository paths do. A pattern without glob
// characters names one repository and matches its path only, never the
// paths beneath it: an exclude entry without them behaves as a plain
// repository reference.

// globChars are the characters that make a pattern a glob.
const globChars = "*?"

func isGlobChar(c rune) bool { return strings.ContainsRune(globChars, c) }

// isGlob reports whether pattern holds glob characters.
func isGlob(pattern string) bool { return strings.ContainsAny(pattern, globChars) }

// matchPath reports whether the repository path p matches pattern.
func matchPath(pattern, p string) bool {
	if !isGlob(pattern) {
		return strings.EqualFold(pattern, p)
	}
	return pathx.MatchSegments(pathx.Fold(pattern), pathx.Fold(p))
}

// matchAnyPath reports whether p matches one of patterns.
func matchAnyPath(patterns []string, p string) bool {
	for _, pat := range patterns {
		if matchPath(pat, p) {
			return true
		}
	}
	return false
}

// checkMatchPattern validates a pattern of match: a full repository path
// with glob characters, without a provider prefix.
func checkMatchPattern(s string) error {
	switch {
	case len(s) > maxRefLen:
		return fmt.Errorf("%.40q…: longer than %d bytes", displayURL(s), maxRefLen)
	case isURL(s):
		return fmt.Errorf("%q: a URL is not accepted here; match takes repository paths such as acme/svc-*", displayURL(s))
	case strings.Contains(s, ":"):
		return fmt.Errorf("%q: match takes repository paths without a provider, such as acme/svc-*", s)
	}
	if err := checkRefPath(s, 2, true); err != nil {
		return fmt.Errorf("%q: %w", s, err)
	}
	return nil
}

// globUnder reports whether a repository under namespace ns can match
// pattern: directly under it, or, with subgroups, at any depth. It is
// what check uses to warn about a match pattern that selects nothing.
func globUnder(pattern, ns string, subgroups bool) bool {
	pat := strings.Split(pathx.Fold(pattern), "/")
	segs := strings.Split(pathx.Fold(ns), "/")
	// at[i]: pat[:i] matches the whole of segs.
	at := globPrefixes(pat, segs)
	for i, ok := range at {
		if !ok {
			continue
		}
		rest := pat[i:]
		// A "**" that took the last namespace segment can take the
		// repository's segments too, when the rest matches nothing.
		if i > 0 && pat[i-1] == "**" && onlyStars(rest) {
			return true
		}
		literal := 0
		for _, s := range rest {
			if s != "**" {
				literal++
			}
		}
		switch {
		case len(rest) == 0:
		case subgroups:
			return true
		case literal <= 1:
			return true
		}
	}
	return false
}

// globPrefixes returns, for each i from 0 to len(pat), whether pat[:i]
// matches segs as a whole.
func globPrefixes(pat, segs []string) []bool {
	n := len(segs)
	prev := make([]bool, n+1) // pat[:i-1] matches segs[:j]
	prev[0] = true
	out := make([]bool, len(pat)+1)
	out[0] = n == 0
	for i := 1; i <= len(pat); i++ {
		cur := make([]bool, n+1)
		for j := 0; j <= n; j++ {
			if pat[i-1] == "**" {
				cur[j] = prev[j] || (j > 0 && cur[j-1])
			} else {
				cur[j] = j > 0 && prev[j-1] && pathx.MatchSegments(pat[i-1], segs[j-1])
			}
		}
		out[i] = cur[n]
		prev = cur
	}
	return out
}

// onlyStars reports whether every segment of pat is "**".
func onlyStars(pat []string) bool {
	for _, s := range pat {
		if s != "**" {
			return false
		}
	}
	return true
}
