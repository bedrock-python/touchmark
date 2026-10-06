package config

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Messages about a config file can quote what the file holds, and the
// opt-in file comes from targets: it is untrusted input that ends up in
// terminals and CI logs. Every error and warning the parsers return is
// cleaned first: control characters (newlines included), bidirectional
// overrides and invalid UTF-8 are escaped, so a value cannot forge a log
// line, a CI annotation (::error ...) or a terminal escape; whatever
// looks like a credential is cut to its prefix; and the user information
// of a URL (user:password@) is cut.

// cleanMessage returns s safe to print.
func cleanMessage(s string) string { return redactUserinfo(redactSecrets(escapeUnprintable(s))) }

// userinfoRe matches the scheme and user information of a URL: everything
// between "://" and the "@" that ends the user information.
var userinfoRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/?#@\s"'\x60]*@`)

// redactUserinfo cuts the user information of every URL in s.
func redactUserinfo(s string) string { return userinfoRe.ReplaceAllString(s, "${1}…@") }

// escapeUnprintable escapes, Go style, every rune of s that could change
// how the text around it is displayed.
func escapeUnprintable(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, needsEscape) < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteString(`\x` + strconv.FormatUint(uint64(s[i]), 16))
		case needsEscape(r):
			q := strconv.QuoteRuneToASCII(r)
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// needsEscape reports control and format characters: C0, C1, and the
// invisible ones such as bidirectional overrides and zero-width spaces.
func needsEscape(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// redactSecrets cuts every match of secretRe in s down to its prefix. A PEM
// block has no reliable end inside a message, so everything after its
// header goes.
func redactSecrets(s string) string {
	matches := secretRe.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		start, end := secretPrefix(m)
		b.WriteString(s[last:end])
		b.WriteString("…")
		last = m[1]
		if s[start:end] == "-----BEGIN" {
			return b.String()
		}
	}
	b.WriteString(s[last:])
	return b.String()
}

// cleanErr returns err with every message cleaned. Joined errors stay
// joined, one cleaned error each; an error whose message needs no change
// is kept as it is, with its wrapped chain.
func cleanErr(err error) error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var errs []error
		for _, e := range j.Unwrap() {
			errs = append(errs, cleanErr(e))
		}
		return errors.Join(errs...)
	}
	msg := err.Error()
	if clean := cleanMessage(msg); clean != msg {
		return errors.New(clean)
	}
	return err
}

// cleanWarnings returns warns with every message cleaned.
func cleanWarnings(warns []Warning) []Warning {
	for i := range warns {
		warns[i].Message = cleanMessage(warns[i].Message)
	}
	return warns
}

// yamlError describes an error of the YAML decoder in one line. A type
// error lists one problem per line; they are joined with "; ". The errors
// of a bad alias get a hint: a glob pattern that starts with "*" (**/legacy)
// is read as one unless quoted.
func yamlError(err error) string {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		return "yaml: " + strings.Join(te.Errors, "; ")
	}
	msg := err.Error()
	if strings.Contains(msg, "did not find expected alphabetic or numeric character") || strings.Contains(msg, "unknown anchor") {
		msg += ` (a value that starts with * is read as a YAML alias: quote it, as in "**/legacy")`
	}
	return msg
}
