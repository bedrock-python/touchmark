package config

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/bedrock-python/touchmark/schemas"
)

const (
	testHead40 = "4b1d9e0c5f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c"
	testHead64 = testHead40 + "4b1d9e0c5f3a2b1c0d9e8f7a"
)

func mustParseOperations(t *testing.T, data []byte) *Operations {
	t.Helper()
	o, warns, err := ParseOperations(data)
	if err != nil {
		t.Fatalf("ParseOperations: %v", err)
	}
	if len(warns) > 0 {
		t.Fatalf("ParseOperations: unexpected warnings %v", warns)
	}
	return o
}

func TestParseOperations(t *testing.T) {
	o := mustParseOperations(t, readFixture(t, "operations/valid/example.yml"))
	want := &Operations{
		Version:        1,
		Recreate:       []RecreateOp{{Target: "gh:acme/api", Head: testHead40}},
		ForgetDeclines: []ForgetOp{{Target: "gh:acme/docs", PR: 44}},
		AllowMassClose: &MassCloseOp{Max: 400, Until: "2026-10-01"},
		AdoptUnmarked:  &UntilOp{Until: "2026-10-15"},
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("got  %+v\nwant %+v", o, want)
	}

	o = mustParseOperations(t, readFixture(t, "operations/valid/anchors.yml"))
	wantForget := []ForgetOp{{Target: "gh:acme/api", PR: 44}, {Target: "gh:acme/docs", PR: 45}}
	if !reflect.DeepEqual(o.ForgetDeclines, wantForget) {
		t.Errorf("anchors: forget_declines = %+v, want %+v", o.ForgetDeclines, wantForget)
	}

	o = mustParseOperations(t, readFixture(t, "operations/valid/limits.yml"))
	if o.ForgetDeclines[0].PR != math.MaxInt64 || o.AllowMassClose.Max != math.MaxInt32 {
		t.Errorf("limits = %+v %+v", o.ForgetDeclines, o.AllowMassClose)
	}

	for _, data := range [][]byte{nil, {}, []byte("# nothing\n"), []byte("version: 1\n"), []byte("version: 1\nrecreate:\nallow_mass_close:\n")} {
		o := mustParseOperations(t, data)
		if !reflect.DeepEqual(o, &Operations{Version: 1}) {
			t.Errorf("%q: %+v, want no operations", data, o)
		}
	}
}

func TestParseOperationsReportsEveryProblem(t *testing.T) {
	data := "version: 1\n" +
		"recreate:\n" +
		"  - {target: acme, head: " + testHead40 + "}\n" +
		"  - {target: gh:acme/api, head: abc}\n" +
		"forget_declines:\n" +
		"  - {target: gh:acme/docs, pr: 0}\n" +
		"adopt_unmarked: {until: 2026-02-30}\n"
	o, _, err := ParseOperations([]byte(data))
	if o != nil || err == nil {
		t.Fatalf("ParseOperations = %+v, %v; want an error", o, err)
	}
	want := []string{
		`.touchmark/operations.yml: recreate[0].target: "acme": a repository path needs an owner and a name`,
		`.touchmark/operations.yml: recreate[1].head: "abc" must be a full commit id`,
		`.touchmark/operations.yml: forget_declines[0].pr: must be a positive pull request number, got 0`,
		`.touchmark/operations.yml: adopt_unmarked.until: "2026-02-30" is not a calendar date`,
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != len(want) {
		t.Fatalf("error has %d lines, want %d:\n%v", len(lines), len(want), err)
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], w)
		}
	}
}

func TestParseOperationsWarnings(t *testing.T) {
	o, warns, err := ParseOperations(readFixture(t, "operations/valid/duplicates.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Recreate) != 3 || len(o.ForgetDeclines) != 3 {
		t.Errorf("duplicates are dropped: %+v", o)
	}
	want := []Warning{
		{File: OperationsFile, Message: "recreate[1]: same target and head as recreate[0]; one entry is enough"},
		{File: OperationsFile, Message: "forget_declines[2]: same target and pull request as forget_declines[0]; one entry is enough"},
	}
	if !reflect.DeepEqual(warns, want) {
		t.Errorf("warnings = %v, want %v", warns, want)
	}
	// Warnings come with the error when there is one.
	_, warns, err = ParseOperations([]byte("recreate: [{target: acme/x, head: nope}]\n"))
	if err == nil || len(warns) != 1 || !strings.Contains(warns[0].Message, "no version") {
		t.Errorf("warnings %v, error %v; want the version warning and an error", warns, err)
	}
}

// TestParseOperationsSecrets: a token or a key anywhere in the file is the
// only error, and no message quotes it.
func TestParseOperationsSecrets(t *testing.T) {
	body := strings.Repeat("A1b2", 9)
	for _, tc := range []struct{ name, text, kind string }{
		{"comment", "version: 1\n# ghp_" + body + "\n", "ghp_"},
		{"head", "version: 1\nrecreate: [{target: gh:acme/api, head: ghs_" + body + "}]\n", "ghs_"},
		{"target", "version: 1\nforget_declines: [{target: glpat-" + body + ", pr: 1}]\n", "glpat-"},
		{"unknown key", "version: 1\ngithub_pat_11" + body + ": 1\n", "github_pat_"},
		{"key", "version: 1\n# -----" + "BEGIN OPENSSH PRIVATE KEY-----\n# " + body + "\n", "-----BEGIN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseOperations([]byte(tc.text))
			if err == nil || !strings.Contains(err.Error(), "looks like a secret ("+tc.kind) {
				t.Fatalf("error %v, want a secret finding", err)
			}
			if strings.Contains(err.Error(), body[:12]) || strings.Count(err.Error(), "\n") > 0 {
				t.Errorf("the error quotes the file or reports more: %v", err)
			}
		})
	}
}

// TestParseOperationsEscapes: values from the file cannot forge log lines
// or terminal escapes.
func TestParseOperationsEscapes(t *testing.T) {
	for _, text := range []string{
		"recreate: [{target: gh:acme/api, head: \"\\n::error::forged\\e[2K\"}]\n",
		"adopt_unmarked: {until: \"2026\\u202e-10-01\"}\n",
		"recreate: [{target: \"gh:acme/\\napi\", head: " + testHead40 + "}]\n",
		"\"x\\ny\": 1\n",
	} {
		_, warns, err := ParseOperations([]byte(text))
		if err == nil {
			t.Errorf("%q: want an error", text)
			continue
		}
		msg := err.Error()
		for _, line := range strings.Split(msg, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "::") {
				t.Errorf("%q: a message line starts a CI annotation: %q", text, msg)
			}
		}
		if strings.ContainsAny(msg, "\x1b\u202e") {
			t.Errorf("%q: the message holds a raw escape: %q", text, msg)
		}
		for _, w := range warns {
			if strings.ContainsFunc(w.Message, unicode.IsControl) {
				t.Errorf("%q: warning %q holds a control character", text, w.Message)
			}
		}
	}
	// A long malformed head is cut in the message.
	_, _, err := ParseOperations([]byte("recreate: [{target: acme/api, head: " + strings.Repeat("z", 5000) + "}]\n"))
	if err == nil || len(err.Error()) > 400 || !strings.Contains(err.Error(), "…") {
		t.Errorf("long head: %d bytes: %.300v", len(err.Error()), err)
	}
}

func TestOperationsActiveAndExpired(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		until  string
		now    string
		active bool
	}{
		{"2026-10-01", "2026-09-30T12:00:00Z", true},
		{"2026-10-01", "2026-10-01T00:00:00Z", true},
		{"2026-10-01", "2026-10-01T23:59:59Z", true},
		{"2026-10-01", "2026-10-02T00:00:00Z", false},
		{"2026-10-01", "2027-01-01T00:00:00Z", false},
		// The date is UTC: 01:30 on the 2nd in Berlin is still the 1st in UTC.
		{"2026-10-01", "2026-10-02T01:30:00+02:00", true},
		{"2026-10-01", "2026-10-01T20:00:00-05:00", false},
		{"2028-02-29", "2028-02-29T23:00:00Z", true},
		{"2026-02-30", "2020-01-01T00:00:00Z", false},
		{"", "2020-01-01T00:00:00Z", false},
		{"soon", "2020-01-01T00:00:00Z", false},
	} {
		now := at(tc.now)
		if got := (&UntilOp{Until: tc.until}).Active(now); got != tc.active {
			t.Errorf("UntilOp{%q}.Active(%s) = %v, want %v", tc.until, tc.now, got, tc.active)
		}
		if got := (&MassCloseOp{Max: 1, Until: tc.until}).Active(now); got != tc.active {
			t.Errorf("MassCloseOp{%q}.Active(%s) = %v, want %v", tc.until, tc.now, got, tc.active)
		}
		ops := &Operations{AdoptUnmarked: &UntilOp{Until: tc.until}}
		wantExpired := !tc.active && dateRe.MatchString(tc.until) && tc.until != "2026-02-30"
		if got := len(ops.Expired(now)) == 1; got != wantExpired {
			t.Errorf("Expired(%q at %s) = %v, want expired=%v", tc.until, tc.now, ops.Expired(now), wantExpired)
		}
	}

	ops := mustParseOperations(t, readFixture(t, "operations/valid/example.yml"))
	if w := ops.Expired(at("2026-10-01T12:00:00Z")); len(w) != 0 {
		t.Errorf("before both dates: %v", w)
	}
	w := ops.Expired(at("2026-10-02T00:00:00Z"))
	if len(w) != 1 || w[0].String() != ".touchmark/operations.yml: allow_mass_close: until 2026-10-01 has passed, the entry does nothing; remove it" {
		t.Errorf("after allow_mass_close: %v", w)
	}
	w = ops.Expired(at("2026-12-01T00:00:00Z"))
	if len(w) != 2 || !strings.HasPrefix(w[1].Message, "adopt_unmarked: until 2026-10-15 has passed") {
		t.Errorf("after both: %v", w)
	}
	var none *Operations
	if w := none.Expired(time.Now()); w != nil {
		t.Errorf("nil operations: %v", w)
	}
	if (&Operations{}).AllowMassClose.Active(time.Now()) || (&Operations{}).AdoptUnmarked.Active(time.Now()) {
		t.Error("an absent entry is active")
	}
}

func TestCheckOperations(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ops := func(targets ...string) *Operations {
		o := &Operations{Version: 1}
		for i, target := range targets {
			if i%2 == 0 {
				o.Recreate = append(o.Recreate, RecreateOp{Target: target, Head: testHead40})
			} else {
				o.ForgetDeclines = append(o.ForgetDeclines, ForgetOp{Target: target, PR: int64(i)})
			}
		}
		return o
	}
	tests := []struct {
		name     string
		ops      *Operations
		hub      *Hub
		targets  *Targets
		errors   []string
		warnings []string
	}{
		{name: "nil operations", hub: selectHub("gh")},
		{
			name: "prefixes name providers",
			ops:  ops("gh:acme/api", "corp:platform/docs"),
			hub:  selectHub("gh", "corp"),
		},
		{
			name: "unknown prefixes",
			ops:  ops("gl:acme/api", "corp:platform/docs", "ghe:acme/web"),
			hub:  selectHub("gh", "corp"),
			errors: []string{
				`.touchmark/operations.yml: recreate[0].target: unknown provider "gl" (hub.yml defines gh, corp)`,
				`.touchmark/operations.yml: recreate[1].target: unknown provider "ghe" (hub.yml defines gh, corp)`,
			},
		},
		{
			name: "a bare path with several providers needs defaults.provider",
			ops:  ops("acme/api", "platform/docs"),
			hub:  selectHub("gh", "corp"),
			errors: []string{
				`.touchmark/operations.yml: recreate[0].target: no provider, and hub.yml defines gh, corp; write the target as <provider>:acme/api`,
				`.touchmark/operations.yml: forget_declines[0].target: no provider, and hub.yml defines gh, corp`,
			},
		},
		{
			name:    "defaults.provider resolves a bare path",
			ops:     ops("acme/api", "platform/docs"),
			hub:     selectHub("gh", "corp"),
			targets: &Targets{Defaults: Defaults{Provider: "corp"}},
		},
		{
			name: "the only provider resolves a bare path",
			ops:  ops("acme/api", "gh:acme/docs"),
			hub:  selectHub("gh"),
		},
		{
			name: "a hub without providers takes bare paths only",
			ops:  ops("acme/api", "github:acme/docs"),
			hub:  &Hub{ID: "acme-eng"},
			errors: []string{
				`.touchmark/operations.yml: forget_declines[0].target: unknown provider "github" (hub.yml defines none)`,
			},
		},
		{
			name: "nil hub and targets: a legacy hub",
			ops:  ops("acme/api"),
		},
		{
			name: "malformed targets built in code",
			ops:  ops("acme", "gh:"),
			hub:  selectHub("gh"),
			errors: []string{
				`.touchmark/operations.yml: recreate[0].target: "acme": a repository path needs an owner and a name`,
				`.touchmark/operations.yml: forget_declines[0].target: "gh:": empty path`,
			},
		},
		{
			name: "expired entries are warnings",
			ops: &Operations{
				AllowMassClose: &MassCloseOp{Max: 3, Until: "2026-10-04"},
				AdoptUnmarked:  &UntilOp{Until: "2026-10-05"},
			},
			hub:      selectHub("gh"),
			warnings: []string{".touchmark/operations.yml: allow_mass_close: until 2026-10-04 has passed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warns, errs := CheckOperations(tt.ops, tt.targets, tt.hub, now)
			if len(errs) != len(tt.errors) {
				t.Errorf("errors = %q, want %d", errs, len(tt.errors))
			}
			for i := 0; i < len(errs) && i < len(tt.errors); i++ {
				if !strings.HasPrefix(errs[i].Error(), tt.errors[i]) {
					t.Errorf("error %d = %q, want prefix %q", i, errs[i], tt.errors[i])
				}
			}
			if len(warns) != len(tt.warnings) {
				t.Errorf("warnings = %v, want %d", warns, len(tt.warnings))
			}
			for i := 0; i < len(warns) && i < len(tt.warnings); i++ {
				if !strings.HasPrefix(warns[i].String(), tt.warnings[i]) {
					t.Errorf("warning %d = %q, want prefix %q", i, warns[i], tt.warnings[i])
				}
			}
		})
	}
}

// TestCheckOperationsFixtures: the full hub.yml and the example
// operations.yml belong together.
func TestCheckOperationsFixtures(t *testing.T) {
	hub := mustParseHub(t, readFixture(t, "hub/valid/full.yml"))
	targets, _, err := ParseTargets([]byte("version: 1\ndefaults: {provider: gh}\n"))
	if err != nil {
		t.Fatal(err)
	}
	ops := mustParseOperations(t, readFixture(t, "operations/valid/example.yml"))
	warns, errs := CheckOperations(ops, targets, hub, time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
	if len(warns) > 0 || len(errs) > 0 {
		t.Errorf("warnings %v, errors %v", warns, errs)
	}
	ops = mustParseOperations(t, readFixture(t, "operations/valid/sha256-head.yml"))
	if _, errs := CheckOperations(ops, targets, hub, time.Now()); len(errs) > 0 {
		t.Errorf("corp target: %v", errs)
	}
}

// TestOperationsSchemaIntegerLimits: the schema bounds pr at the int64 the
// parser decodes into. JSON numbers lose precision as float64, so this
// reads the schema with json.Number.
func TestOperationsSchemaIntegerLimits(t *testing.T) {
	data, ok := schemas.Get("operations")
	if !ok {
		t.Fatal("no operations schema")
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	got, ok := lookup(doc, "/properties/forget_declines/items/properties/pr/maximum")
	if !ok || got.(json.Number).String() != "9223372036854775807" {
		t.Errorf("pr maximum = %v, want math.MaxInt64", got)
	}
}

// FuzzParseOperations checks that no input makes ParseOperations, Expired
// or CheckOperations panic, and that what the parser accepts holds the
// rules it promises.
func FuzzParseOperations(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "operations", "*", "*.yml"))
	for _, file := range files {
		if data, err := os.ReadFile(file); err == nil {
			f.Add(data)
		}
	}
	hub := selectHub("gh", "corp")
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte) {
		o, _, err := ParseOperations(data)
		if err != nil {
			if o != nil {
				t.Fatalf("an error and operations: %+v", o)
			}
			return
		}
		if o.Version != 1 {
			t.Fatalf("version %d accepted", o.Version)
		}
		for _, r := range o.Recreate {
			if _, err := ParseRef(r.Target); err != nil || !headRe.MatchString(r.Head) {
				t.Fatalf("accepted recreate %+v", r)
			}
		}
		for _, fd := range o.ForgetDeclines {
			if _, err := ParseRef(fd.Target); err != nil || fd.PR < 1 {
				t.Fatalf("accepted forget_declines %+v", fd)
			}
		}
		if m := o.AllowMassClose; m != nil {
			if _, err := parseDate(m.Until); err != nil || m.Max < 1 || m.Max > maxMassClose {
				t.Fatalf("accepted allow_mass_close %+v", m)
			}
		}
		if a := o.AdoptUnmarked; a != nil {
			if _, err := parseDate(a.Until); err != nil {
				t.Fatalf("accepted adopt_unmarked %+v", a)
			}
		}
		_ = o.Expired(now)
		_, _ = CheckOperations(o, nil, hub, now)
	})
}
