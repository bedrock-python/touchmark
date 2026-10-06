package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// Patterns shared with the JSON Schemas in schemas/. The schema tests check
// that the schemas carry exactly these strings, so an editor and touchmark
// accept the same values.
//
// Whitespace is spelled out (space) instead of \s and \S: JSON Schema
// patterns are ECMA-262 regular expressions, whose \s covers Unicode spaces
// such as NBSP, while Go's \s is ASCII only. A class of literal characters
// means the same in both dialects.
const (
	// space holds the characters ECMA-262 \s matches, as literal characters
	// for a character class.
	space = "\t\n\v\f\r " + string(rune(0x00a0)) + string(rune(0x1680)) +
		string(rune(0x2000)) + "-" + string(rune(0x200a)) + string(rune(0x2028)) +
		string(rune(0x2029)) + string(rune(0x202f)) + string(rune(0x205f)) +
		string(rune(0x3000)) + string(rune(0xfeff))
	packNamePattern   = `^[a-z0-9]+(-[a-z0-9]+)*$`
	providerIDPattern = `^[a-z][a-z0-9-]{0,31}$`
	// refSegment is one path segment of a repository or namespace: the
	// characters platforms allow in names, and never "." or "..".
	refSegment       = `([A-Za-z0-9_-][A-Za-z0-9_.-]*|\.[A-Za-z0-9_-][A-Za-z0-9_.-]*|\.\.[A-Za-z0-9_.-]+)`
	refPrefix        = `^([a-z][a-z0-9-]{0,31}:)?`
	refPattern       = refPrefix + refSegment + `(/` + refSegment + `)+$`
	namespacePattern = refPrefix + refSegment + `(/` + refSegment + `)*$`
	// urlBase is the scheme, host and port of a URL touchmark accepts:
	// https, or http for the loopback host only.
	urlBase    = `(https://[A-Za-z0-9.-]+(:[0-9]{1,5})?|http://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]{1,5})?)`
	urlPattern = `^` + urlBase + `(/[^?#` + space + `]*)?$`
	// globSegment is one segment of an exclude or match pattern: a
	// repository path segment that may hold the glob characters * and ?.
	// A segment of "**" spans segments.
	globSegment = `([A-Za-z0-9_*?-][A-Za-z0-9_.*?-]*|\.[A-Za-z0-9_*?-][A-Za-z0-9_.*?-]*|\.\.[A-Za-z0-9_.*?-]+)`
	// urlGlobSegment is globSegment without ?, which starts a URL's query.
	urlGlobSegment = `([A-Za-z0-9_*-][A-Za-z0-9_.*-]*|\.[A-Za-z0-9_*-][A-Za-z0-9_.*-]*|\.\.[A-Za-z0-9_.*-]+)`
	// excludePattern is an exclude entry written as [<provider>:]<pattern>,
	// matchPattern a pattern of match: both cover a repository path, two
	// segments or more.
	excludePattern = refPrefix + globSegment + `(/` + globSegment + `)+$`
	matchPattern   = `^` + globSegment + `(/` + globSegment + `)+$`
	// The web URLs targets.yml accepts: a repository's (repo), a
	// namespace's (org and group), and an exclude pattern's. A provider may
	// live under a path, so how many segments name the repository is known
	// only once the URL meets the providers (ResolveURLs); a trailing slash
	// and, for a repository, a trailing .git are allowed.
	repoURLPattern      = `^` + urlBase + `(/` + refSegment + `){2,}/?$`
	namespaceURLPattern = `^` + urlBase + `(/` + refSegment + `)+/?$`
	excludeURLPattern   = `^` + urlBase + `(/` + urlGlobSegment + `){2,}/?$`
	fingerprintPattern  = `^[A-Za-z0-9.-]+(:[0-9]{1,5})?/[0-9]+$`
	cooldownPattern     = `^[1-9][0-9]{0,4}[dh]$`
	accountPattern      = `^[^` + space + `]+$`
	labelPattern        = `^[^,]*[^,` + space + `][^,]*$`
	nonBlankPattern     = `[^` + space + `]`
	// patternPattern requires an ignore or sensitive_paths pattern to hold
	// something besides spaces, slashes and backslashes; the parser checks
	// that it is not empty after trimming them and "./".
	patternPattern = `[^` + space + `/\\]`
	// draftPattern matches the title prefixes that turn a GitLab merge
	// request into a draft. A commit message is the default MR title.
	draftPattern = `^[` + space + `]*([Dd][Rr][Aa][Ff][Tt]:|\[[Dd][Rr][Aa][Ff][Tt]\]|\([Dd][Rr][Aa][Ff][Tt]\)|[Ww][Ii][Pp]:|\[[Ww][Ii][Pp]\])`
)

// Length limits, also in the schemas.
const (
	maxPackNameLen = 64
	minHubIDLen    = 3
	maxHubIDLen    = 40
	maxRefLen      = 512
	maxLabelLen    = 50
	maxAccountLen  = 255
	// maxIgnorePatterns bounds the opt-in file's ignore list: every pattern
	// is matched against every managed path.
	maxIgnorePatterns = 1000
)

var (
	packNameRe     = regexp.MustCompile(packNamePattern)
	providerIDRe   = regexp.MustCompile(providerIDPattern)
	urlRe          = regexp.MustCompile(urlPattern)
	repoURLRe      = regexp.MustCompile(repoURLPattern)
	namespaceURLRe = regexp.MustCompile(namespaceURLPattern)
	excludeURLRe   = regexp.MustCompile(excludeURLPattern)
	fingerprintRe  = regexp.MustCompile(fingerprintPattern)
	cooldownRe     = regexp.MustCompile(cooldownPattern)
	accountRe      = regexp.MustCompile(accountPattern)
	labelRe        = regexp.MustCompile(labelPattern)
	nonBlankRe     = regexp.MustCompile(nonBlankPattern)
	draftRe        = regexp.MustCompile(draftPattern)
)

// secretRe matches strings that look like credentials: GitHub and GitLab
// tokens by their documented prefixes, JSON Web Tokens (GitLab CI job
// tokens, OIDC tokens) and PEM blocks. Each alternative captures its prefix
// in a group, which is all a message may show.
var secretRe = regexp.MustCompile(
	`(?:^|[^A-Za-z0-9_])(ghp_|gho_|ghs_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{20,}` +
		`|(?:^|[^A-Za-z0-9_-])(glpat-|gldt-|glptt-|glrt-|glrtr-|glcbt-|gloas-|glft-|glsoat-|glimt-|glagent-|glffct-)[A-Za-z0-9_.-]{20,}` +
		`|(?:^|[^A-Za-z0-9_-])(eyJ)[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*` +
		`|(-----BEGIN)`)

// problems collects the findings of one file.
type problems struct {
	file  string
	errs  []error
	warns []Warning
}

func (p *problems) errorf(field, format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf("%s: %s", p.file, withField(field, format, args...)))
}

func (p *problems) warnf(field, format string, args ...any) {
	p.warns = append(p.warns, Warning{File: p.file, Message: withField(field, format, args...)})
}

func (p *problems) err() error { return errors.Join(p.errs...) }

func withField(field, format string, args ...any) string {
	msg := fmt.Sprintf(format, args...)
	if field == "" {
		return msg
	}
	return field + ": " + msg
}

// checkVersion validates the version key: absent means 1 (with a warning,
// unless the file is empty), present must be 1.
func checkVersion(version *int, doc document, p *problems) {
	if !doc.has("version") {
		if !doc.empty {
			p.warnf("", "no version, assuming 1")
		}
		*version = 1
		return
	}
	if *version != 1 {
		p.errorf("version", "must be 1, got %d", *version)
	}
}

// checkPackName validates a pack name.
func checkPackName(name string) error {
	switch {
	case name == "":
		return errors.New("empty pack name")
	case len(name) > maxPackNameLen:
		return fmt.Errorf("pack name %q is longer than %d characters", name, maxPackNameLen)
	case !packNameRe.MatchString(name):
		return fmt.Errorf("pack name %q must be lowercase letters and digits in words joined by single hyphens", name)
	}
	return nil
}

func checkPackNames(p *problems, field string, names []string) {
	for i, name := range names {
		if err := checkPackName(name); err != nil {
			p.errorf(fmt.Sprintf("%s[%d]", field, i), "%v", err)
		}
	}
}

// checkProviderID validates a provider id.
func checkProviderID(id string) error {
	if !providerIDRe.MatchString(id) {
		return fmt.Errorf("provider id %q must be a lowercase letter followed by up to 31 lowercase letters, digits or hyphens", id)
	}
	return nil
}

// checkURL validates a provider URL.
func checkURL(s string) error {
	if !urlRe.MatchString(s) {
		return fmt.Errorf("%q must be an https:// URL without credentials, query or fragment (http:// only for localhost)", s)
	}
	return nil
}

// checkAccount validates an account login or slug.
func checkAccount(s string) error {
	if !accountRe.MatchString(s) || utf8.RuneCountInString(s) > maxAccountLen {
		return fmt.Errorf("%q must be an account name without spaces, at most %d characters", s, maxAccountLen)
	}
	return nil
}

// checkPattern validates an ignore or sensitive_paths pattern.
func checkPattern(s string) error {
	if pathx.NormalizePattern(s) == "" {
		return fmt.Errorf("pattern %q is empty after trimming spaces, slashes and ./", s)
	}
	return nil
}

func isBlank(s string) bool { return !nonBlankRe.MatchString(s) }

// scissorsLine is git's scissors line: git cuts a commit message there when
// it reads the message's trailers, so a commit.message holding it would hide
// touchmark's trailers and every sync branch would look
// foreign.
const scissorsLine = "# ------------------------ >8 ------------------------"

// hasScissorsLine reports whether a line of msg is the scissors line, as
// prbody.CommitMessage cleans messages: CR, CRLF and LF all end lines, and
// trailing spaces, tabs, vertical tabs and form feeds do not count.
func hasScissorsLine(msg string) bool {
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	msg = strings.ReplaceAll(msg, "\r", "\n")
	for line := range strings.SplitSeq(msg, "\n") {
		if strings.TrimRight(line, " \t\v\f") == scissorsLine {
			return true
		}
	}
	return false
}

// checkBranchName validates a git branch name with the rules of
// git check-ref-format, plus no leading '-'.
func checkBranchName(s string) error {
	bad := func(why string) error { return fmt.Errorf("branch name %q: %s", s, why) }
	switch {
	case s == "":
		return bad("empty")
	case s == "@":
		return bad(`"@" is not a branch name`)
	case strings.HasPrefix(s, "-"):
		return bad("starts with '-'")
	case strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.Contains(s, "//"):
		return bad("empty component")
	case strings.HasSuffix(s, "."):
		return bad("ends with '.'")
	case strings.Contains(s, ".."):
		return bad(`contains ".."`)
	case strings.Contains(s, "@{"):
		return bad(`contains "@{"`)
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			return bad(fmt.Sprintf("character %q is not allowed", r))
		}
	}
	for _, c := range strings.Split(s, "/") {
		if strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return bad("a component starts with '.' or ends with .lock")
		}
	}
	return nil
}

// parseRef parses "path" or "provider:path" where path has at least
// minSegments segments.
func parseRef(s string, minSegments int) (Ref, error) { return parseRefOf(s, minSegments, false) }

// parsePattern parses an exclude entry that is not a URL:
// "[provider:]pattern", where pattern covers two segments or more and may
// hold the glob characters * and ? (glob.go). An entry without them is a
// reference, which ParseRef reads.
func parsePattern(s string) (Ref, error) { return parseRefOf(s, 2, isGlob(s)) }

// parseRefOf is parseRef, with the glob characters * and ? allowed in the
// path when glob is set. A URL is refused: only targets.yml takes one, and
// ResolveURLs rewrites it first.
func parseRefOf(s string, minSegments int, glob bool) (Ref, error) {
	if len(s) > maxRefLen {
		return Ref{}, fmt.Errorf("%.40q…: longer than %d bytes", displayURL(s), maxRefLen)
	}
	if isURL(s) {
		return Ref{}, fmt.Errorf("%q: a URL is not accepted here; write the repository as <provider>:<path>", displayURL(s))
	}
	var r Ref
	path := s
	if prov, rest, ok := strings.Cut(s, ":"); ok {
		if err := checkProviderID(prov); err != nil {
			return Ref{}, fmt.Errorf("%q: %w", s, err)
		}
		r.Provider, path = prov, rest
	}
	if err := checkRefPath(path, minSegments, glob); err != nil {
		return Ref{}, fmt.Errorf("%q: %w", s, err)
	}
	r.Path = path
	return r, nil
}

// checkRefPath checks a repository or namespace path of at least
// minSegments segments; with glob set, a segment may hold * and ?.
func checkRefPath(p string, minSegments int, glob bool) error {
	if p == "" {
		return errors.New("empty path")
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return errors.New("leading or trailing slash")
	}
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		switch seg {
		case "":
			return errors.New("empty path segment")
		case ".", "..":
			return fmt.Errorf("%q path segment", seg)
		}
		for _, c := range seg {
			switch {
			case isRefChar(c), glob && isGlobChar(c):
			case isGlobChar(c):
				return fmt.Errorf("character %q is not allowed in a repository path; glob patterns go in exclude and match", c)
			default:
				return fmt.Errorf("character %q is not allowed in a repository path", c)
			}
		}
	}
	if len(segs) < minSegments {
		if glob {
			return errors.New("a pattern covers a whole repository path, like acme/legacy-* or platform/legacy/**")
		}
		return errors.New("a repository path needs an owner and a name, like acme/billing")
	}
	return nil
}

func isRefChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '.'
}

// scanSecrets reports every string in data that looks like a credential and
// whether it found one. The message names the line and the kind, never the
// value.
func scanSecrets(p *problems, data []byte) bool {
	matches := secretRe.FindAllSubmatchIndex(data, -1)
	for _, m := range matches {
		start, end := secretPrefix(m)
		line := 1 + bytes.Count(data[:m[0]], []byte("\n"))
		p.errorf("", "line %d looks like a secret (%s…); remove it from the file and revoke it", line, data[start:end])
	}
	return len(matches) > 0
}

// secretPrefix returns the span of the prefix group that matched in a
// secretRe submatch index.
func secretPrefix(m []int) (start, end int) {
	for g := 1; 2*g+1 < len(m); g++ {
		if m[2*g] >= 0 {
			return m[2*g], m[2*g+1]
		}
	}
	return m[0], m[0]
}
