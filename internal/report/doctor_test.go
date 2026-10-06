package report

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/bedrock-python/touchmark/schemas"
)

// sampleDoctor is a doctor report with a check of every status, a skipped
// target and a target a public hub does not name.
func sampleDoctor() *Doctor {
	d := NewDoctor("0.2.0")
	d.Hub = DeliveryHub{ID: "acme-eng", Fingerprint: "github.com/712345678", Commit: sampleCommit}
	d.HubChecks = []DoctorCheck{
		{Name: "write-isolation", Status: StatusOK, Detail: "the probe job sees no write key"},
		{Name: "environment", Status: StatusFail, Detail: "the environment lets any branch use it"},
	}
	d.Providers = []DoctorProvider{{
		ID: "gh", Type: "github", Host: "github.com", Writer: "acme-write[bot]", Self: "acme-write[bot]", ResolveComplete: true,
		Checks: []DoctorCheck{
			{Name: "writer", Status: StatusOK, Detail: "the write credential acts as acme-write[bot]"},
			{Name: "token-expiry", Status: StatusUnknown, Detail: "not read"},
		},
	}}
	d.Targets = []DoctorTarget{
		{Provider: "gh", Host: "github.com", RepoID: "2", Path: "acme/web", Skipped: "not-opted-in"},
		{Provider: "gh", Host: "github.com", Checks: []DoctorCheck{{Name: "access", Status: StatusFail}}},
		{Provider: "gh", Host: "github.com", RepoID: "1", Path: "acme/api", Checks: []DoctorCheck{
			{Name: "access", Status: StatusOK, Detail: "may push"},
			{Name: "rules", Status: StatusWarn, Detail: "force pushes blocked on touchmark/acme-eng"},
		}},
	}
	d.Warnings = []string{"provider gh: only pull requests by known_authors are recognized as touchmark's"}
	d.Summarize()
	return d
}

// hiddenOf returns the target of d that the report does not name.
func hiddenOf(d *Doctor) *DoctorTarget {
	for i := range d.Targets {
		if d.Targets[i].Path == "" {
			return &d.Targets[i]
		}
	}
	return nil
}

// compileDoctorSchema compiles schemas/doctor.schema.json.
func compileDoctorSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	data, ok := schemas.Get("doctor")
	if !ok {
		t.Fatal("no doctor schema")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	url := schemas.BaseURL + "doctor.schema.json"
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func validateDoctor(sch *jsonschema.Schema, d *Doctor) error {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, d); err != nil {
		return err
	}
	v, err := jsonschema.UnmarshalJSON(&buf)
	if err != nil {
		return err
	}
	return sch.Validate(v)
}

func TestDoctorSchema(t *testing.T) {
	sch := compileDoctorSchema(t)
	empty := NewDoctor("dev")
	empty.Hub = DeliveryHub{ID: "acme-eng", Fingerprint: "gitlab.example.com:8443/1234", Commit: sampleCommit}
	for name, d := range map[string]*Doctor{"sample": sampleDoctor(), "empty": empty} {
		if err := validateDoctor(sch, d); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]func(*Doctor){
		"status":   func(d *Doctor) { d.HubChecks[0].Status = "red" },
		"check":    func(d *Doctor) { d.HubChecks[0].Name = "Write Isolation" },
		"provider": func(d *Doctor) { d.Providers[0].Type = "bitbucket" },
		"summary":  func(d *Doctor) { delete(d.Summary, StatusUnknown) },
		"schema":   func(d *Doctor) { d.Schema = "doctor/v2" },
	}
	for name, edit := range bad {
		d := sampleDoctor()
		edit(d)
		if validateDoctor(sch, d) == nil {
			t.Errorf("%s: the schema accepts it", name)
		}
	}
}

func TestDoctorExitCode(t *testing.T) {
	d := sampleDoctor()
	if got := d.ExitCode(); got != exitFailed {
		t.Errorf("a failed check: exit %d", got)
	}
	d.HubChecks[1].Status = StatusOK
	hiddenOf(d).Checks[0].Status = StatusOK
	if got := d.ExitCode(); got != exitOK {
		t.Errorf("warnings without --strict: exit %d", got)
	}
	d.Strict = true
	if got := d.ExitCode(); got != exitStrict {
		t.Errorf("warnings with --strict: exit %d", got)
	}
	d.Providers[0].Error = "probe: auth"
	if got := d.ExitCode(); got != exitFailed {
		t.Errorf("a provider unavailable: exit %d", got)
	}
}

// The outputs count the target a public hub does not name, and give none
// of its details; the matrix and the problems name the others.
func TestDoctorOutputs(t *testing.T) {
	d := sampleDoctor()
	// A detail a driver left on the hidden target would still not print.
	hiddenOf(d).Checks[0].Detail = "acme/secret-roadmap is gone"
	var text, md bytes.Buffer
	if err := d.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"text": text.String(), "markdown": md.String()} {
		if strings.Contains(out, "secret-roadmap") {
			t.Errorf("%s names the hidden target:\n%s", name, out)
		}
		for _, s := range []string{"acme/api", "private in a public hub, not named", "fail 1", "force pushes blocked", "not\\-opted\\-in 1"} {
			if name == "text" {
				s = strings.ReplaceAll(s, `\`, "")
			}
			if !strings.Contains(out, s) {
				t.Errorf("%s lacks %q:\n%s", name, s, out)
			}
		}
	}
	if !strings.Contains(text.String(), "Summary  ok 3 · warn 1 · fail 2 · unknown 1") {
		t.Errorf("text:\n%s", text.String())
	}
}

// A detail that holds line breaks prints on one line: a line of its own in
// a CI log could pass for a workflow command.
func TestDoctorTextOneLine(t *testing.T) {
	d := sampleDoctor()
	d.Providers[0].Checks[1].Detail = "first\n::error::spoofed\r\nlast"
	var b bytes.Buffer
	if err := d.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "::") {
			t.Errorf("a line starts a workflow command: %q", line)
		}
	}
}

// WriteSummary cuts a report to its limit, with a note, and writes nothing
// without room.
func TestSummaryLimit(t *testing.T) {
	d := NewDelivery("distribute", "test")
	for i := range 3000 {
		d.Targets = append(d.Targets, DeliveryTarget{Provider: "gh", Host: "github.com", RepoID: fmt.Sprint(i), Path: fmt.Sprintf("acme/repository-%05d", i), Outcome: OutcomeOpened})
	}
	d.Summarize()
	for _, limit := range []int{minSummary, 20 << 10, 64 << 10} {
		var b bytes.Buffer
		if err := d.WriteSummary(&b, limit); err != nil {
			t.Fatal(err)
		}
		if b.Len() > limit || !strings.HasSuffix(b.String(), summaryNote) || !strings.Contains(b.String(), "</details>") {
			t.Errorf("limit %d: %d bytes, ends %q", limit, b.Len(), b.String()[max(0, b.Len()-200):])
		}
	}
	var b bytes.Buffer
	if err := d.WriteSummary(&b, minSummary-1); err != nil || b.Len() != 0 {
		t.Errorf("below the minimum: %d bytes, %v", b.Len(), err)
	}
	doc := sampleDoctor()
	b.Reset()
	if err := doc.WriteSummary(&b, 1<<20); err != nil || !strings.Contains(b.String(), "### touchmark doctor") {
		t.Errorf("doctor summary %v:\n%s", err, b.String())
	}
}
