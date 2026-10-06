package config

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// OperationsFile is the hub file of one-off operations that override a
// safeguard. It goes through review like packs.
const OperationsFile = ".touchmark/operations.yml"

// Patterns and limits of operations.yml, shared with
// schemas/operations.schema.json (the schema tests compare them).
const (
	// headPattern is a full commit id: sha1 or sha256, lowercase hex.
	headPattern = `^([0-9a-f]{40}|[0-9a-f]{64})$`
	// datePattern is a date written YYYY-MM-DD. The parser also checks
	// that the day exists in its month, which the schema cannot.
	datePattern = `^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])$`
	// maxMassClose bounds allow_mass_close.max so that it fits an int on
	// every platform.
	maxMassClose = math.MaxInt32
	// maxQuoted is how many characters of a malformed value a message
	// quotes.
	maxQuoted  = 70
	dateLayout = "2006-01-02"
)

var (
	headRe = regexp.MustCompile(headPattern)
	dateRe = regexp.MustCompile(datePattern)
)

// Operations is .touchmark/operations.yml. Every entry limits itself (a
// branch head, a PR number, a date), so a forgotten entry does nothing.
type Operations struct {
	Version int `yaml:"version"`
	// Recreate rebuilds a paused sync branch while its head is Head.
	Recreate []RecreateOp `yaml:"recreate"`
	// ForgetDeclines proposes the content of a declined PR again.
	ForgetDeclines []ForgetOp `yaml:"forget_declines"`
	// AllowMassClose lifts the mass-close guard up to Max closes until the
	// date.
	AllowMassClose *MassCloseOp `yaml:"allow_mass_close"`
	// AdoptUnmarked takes over the open PRs without a marker that a
	// multi-gitter setup left on a branch alias, until the date.
	AdoptUnmarked *UntilOp `yaml:"adopt_unmarked"`
}

// RecreateOp targets one branch head.
type RecreateOp struct {
	Target string `yaml:"target"` // "<provider>:<path>" or "<path>"
	Head   string `yaml:"head"`   // full commit id (40 or 64 hex)
}

// ForgetOp targets one declined PR.
type ForgetOp struct {
	Target string `yaml:"target"`
	PR     int64  `yaml:"pr"`
}

// MassCloseOp lifts the mass-close guard.
type MassCloseOp struct {
	Max   int    `yaml:"max"`
	Until string `yaml:"until"` // YYYY-MM-DD, inclusive, UTC
}

// UntilOp is active until a date.
type UntilOp struct {
	Until string `yaml:"until"` // YYYY-MM-DD, inclusive, UTC
}

// Active reports whether the entry is in force at now. Until is inclusive
// and read in UTC: the entry ends at the midnight (UTC) after that day. An
// absent entry (nil) or a malformed date is never active. Max is not looked
// at.
func (m *MassCloseOp) Active(now time.Time) bool { return m != nil && activeUntil(m.Until, now) }

// Active reports whether the entry is in force at now, with the rules of
// MassCloseOp.Active.
func (u *UntilOp) Active(now time.Time) bool { return u != nil && activeUntil(u.Until, now) }

// ParseOperations decodes and validates operations.yml. nil data means the
// file is absent: no operations. Strict like the other files: unknown keys,
// a version other than 1, malformed targets (ParseRef), heads that are not
// full lowercase hex ids, non-positive PR numbers or max, and malformed
// dates are errors. Duplicate entries (same target and head, or same target
// and PR) are warnings.
//
// Like hub.yml and targets.yml: an empty or comment-only file is valid, a
// missing version is 1 with a warning, and a string that looks like a token
// or a private key anywhere in the file is an error found before decoding
// (the only error reported then). Targets compare with their provider
// prefix as written and their path ignoring case; a bare path and the same
// path with a prefix are different targets here, since only the hub knows
// its providers. allow_mass_close.max is at most 2147483647. Messages are
// safe to print, as those of ParseHub.
func ParseOperations(data []byte) (*Operations, []Warning, error) {
	o, warns, err := parseOperations(data)
	return o, cleanWarnings(warns), cleanErr(err)
}

func parseOperations(data []byte) (*Operations, []Warning, error) {
	o := &Operations{Version: 1}
	if data == nil {
		return o, nil, nil
	}
	o.Version = 0
	p := &problems{file: OperationsFile}
	if scanSecrets(p, data) {
		return nil, nil, p.err()
	}
	doc, err := decodeStrict(OperationsFile, data, o)
	if err != nil {
		return nil, nil, err
	}
	validateOperations(o, doc, p)
	if err := p.err(); err != nil {
		return nil, p.warns, err
	}
	return o, p.warns, nil
}

func validateOperations(o *Operations, doc document, p *problems) {
	checkVersion(&o.Version, doc, p)
	heads := map[string]int{}
	for i, r := range o.Recreate {
		field := fmt.Sprintf("recreate[%d]", i)
		ref, ok := checkOpTarget(p, doc, field, r.Target)
		switch {
		case !doc.has(field + ".head"):
			p.errorf(field+".head", "required: the full commit id of the paused branch's head")
		case !headRe.MatchString(r.Head):
			p.errorf(field+".head", "%s must be a full commit id: 40 or 64 lowercase hex digits", quoteShort(r.Head))
		case ok:
			warnDuplicate(p, heads, opTargetKey(ref)+"\x00"+r.Head, field, i, "head")
		}
	}
	prs := map[string]int{}
	for i, f := range o.ForgetDeclines {
		field := fmt.Sprintf("forget_declines[%d]", i)
		ref, ok := checkOpTarget(p, doc, field, f.Target)
		switch {
		case !doc.has(field + ".pr"):
			p.errorf(field+".pr", "required: the number of the declined pull request")
		case f.PR < 1:
			p.errorf(field+".pr", "must be a positive pull request number, got %d", f.PR)
		case ok:
			warnDuplicate(p, prs, fmt.Sprintf("%s\x00%d", opTargetKey(ref), f.PR), field, i, "pull request")
		}
	}
	if m := o.AllowMassClose; m != nil {
		switch {
		case !doc.has("allow_mass_close.max"):
			p.errorf("allow_mass_close.max", "required: how many pull requests a run may close")
		case m.Max < 1 || m.Max > maxMassClose:
			p.errorf("allow_mass_close.max", "must be from 1 to %d, got %d", maxMassClose, m.Max)
		}
		checkUntil(p, doc, "allow_mass_close", m.Until)
	}
	if a := o.AdoptUnmarked; a != nil {
		checkUntil(p, doc, "adopt_unmarked", a.Until)
	}
}

// checkOpTarget validates the target of the entry at field and returns it
// parsed.
func checkOpTarget(p *problems, doc document, field, target string) (Ref, bool) {
	if !doc.has(field + ".target") {
		p.errorf(field+".target", "required: the target as <provider>:<path> or <path>")
		return Ref{}, false
	}
	ref, err := ParseRef(target)
	if err != nil {
		p.errorf(field+".target", "%v", err)
		return Ref{}, false
	}
	return ref, true
}

// opTargetKey identifies a target within operations.yml: the provider
// prefix as written and the path ignoring case, as Select compares paths.
func opTargetKey(r Ref) string { return r.Provider + ":" + strings.ToLower(r.Path) }

// warnDuplicate records key for the entry at index i, or warns that an
// earlier entry had it.
func warnDuplicate(p *problems, seen map[string]int, key, field string, i int, what string) {
	if j, dup := seen[key]; dup {
		section, _, _ := strings.Cut(field, "[")
		p.warnf(field, "same target and %s as %s[%d]; one entry is enough", what, section, j)
		return
	}
	seen[key] = i
}

// checkUntil validates the until date of the section called field.
func checkUntil(p *problems, doc document, field, until string) {
	f := field + ".until"
	if !doc.has(f) {
		p.errorf(f, "required: the last day the entry is active, as YYYY-MM-DD (UTC)")
		return
	}
	if _, err := parseDate(until); err != nil {
		p.errorf(f, "%v", err)
	}
}

// parseDate parses a YYYY-MM-DD date as midnight UTC.
func parseDate(s string) (time.Time, error) {
	if !dateRe.MatchString(s) {
		return time.Time{}, fmt.Errorf("%s must be a date written YYYY-MM-DD", quoteShort(s))
	}
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s is not a calendar date", quoteShort(s))
	}
	return d, nil
}

// untilEnd returns the first instant after the inclusive date until: the
// next midnight UTC.
func untilEnd(until string) (time.Time, error) {
	d, err := parseDate(until)
	if err != nil {
		return time.Time{}, err
	}
	return d.AddDate(0, 0, 1), nil
}

func activeUntil(until string, now time.Time) bool {
	end, err := untilEnd(until)
	return err == nil && now.Before(end)
}

// quoteShort quotes s for a message, cut to maxQuoted characters.
func quoteShort(s string) string {
	if utf8.RuneCountInString(s) > maxQuoted {
		return fmt.Sprintf("%.*q…", maxQuoted, s)
	}
	return fmt.Sprintf("%q", s)
}

// Expired returns warnings for entries whose until date is before now
// (UTC), so `check` can ask for the file to be cleaned up.
//
// An entry expires at the midnight (UTC) after its until date, when Active
// turns false. Entries with a malformed date, which ParseOperations
// rejects, are skipped. A nil Operations has no entries.
func (o *Operations) Expired(now time.Time) []Warning {
	if o == nil {
		return nil
	}
	var warns []Warning
	expired := func(field, until string) {
		end, err := untilEnd(until)
		if err == nil && !now.Before(end) {
			warns = append(warns, Warning{
				File:    OperationsFile,
				Message: fmt.Sprintf("%s: until %s has passed, the entry does nothing; remove it", field, until),
			})
		}
	}
	if o.AllowMassClose != nil {
		expired("allow_mass_close", o.AllowMassClose.Until)
	}
	if o.AdoptUnmarked != nil {
		expired("adopt_unmarked", o.AdoptUnmarked.Until)
	}
	return warns
}
