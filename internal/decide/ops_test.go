package decide

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
)

// Branch heads of the operations tests.
const (
	opsHead1  = "4b1d9e0c5f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c"
	opsHead2  = "5c2e0f1d6a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d"
	opsHead3  = "6d3f1a2e7b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e"
	opsHead64 = "7e4a2b3f8c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f"
)

// Hub, targets and operations files of the operations tests.
const (
	opsTwoProviders = `version: 1
id: acme-eng
providers:
  - id: gh
    type: github
  - id: corp
    type: gitlab
    url: https://gitlab.example.com
`
	opsOneProvider = `version: 1
id: acme-eng
providers:
  - id: gh
    type: github
`
	opsNoProviders = "version: 1\nid: acme-eng\n"

	opsTargetsDefault = "version: 1\ndefaults:\n  provider: gh\ntargets:\n  - repo: acme/api\n"
	opsTargetsBare    = "version: 1\ntargets:\n  - repo: gh:acme/api\n"

	opsFile = `version: 1
recreate:
  - target: gh:acme/api
    head: ` + opsHead1 + `
  - target: acme/api
    head: ` + opsHead2 + `
  - target: corp:acme/api
    head: ` + opsHead3 + `
  - target: gh:ACME/API
    head: ` + opsHead64 + `
forget_declines:
  - target: gh:acme/api
    pr: 44
  - target: acme/api
    pr: 45
  - target: corp:acme/api
    pr: 46
  - target: gh:acme/docs
    pr: 47
allow_mass_close: { max: 400, until: 2026-10-01 }
adopt_unmarked: { until: 2026-10-15 }
`
)

// opsParse parses the three files; "" leaves one out (nil).
func opsParse(t *testing.T, hubYAML, targetsYAML, opsYAML string) (*config.Hub, *config.Targets, *config.Operations) {
	t.Helper()
	var hub *config.Hub
	var targets *config.Targets
	var ops *config.Operations
	var err error
	if hubYAML != "" {
		if hub, _, err = config.ParseHub([]byte(hubYAML)); err != nil {
			t.Fatal(err)
		}
	}
	if targetsYAML != "" {
		if targets, _, err = config.ParseTargets([]byte(targetsYAML)); err != nil {
			t.Fatal(err)
		}
	}
	if opsYAML != "" {
		if ops, _, err = config.ParseOperations([]byte(opsYAML)); err != nil {
			t.Fatal(err)
		}
	}
	return hub, targets, ops
}

func TestOpsFor(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name           string
		hub, targets   string
		provider, path string
		heads          []string
		forget         []int64
	}{
		{
			name: "two providers, defaults.provider gh: gh",
			hub:  opsTwoProviders, targets: opsTargetsDefault, provider: "gh", path: "acme/api",
			heads: []string{opsHead1, opsHead2, opsHead64}, forget: []int64{44, 45},
		},
		{
			name: "two providers, defaults.provider gh: the path in another case",
			hub:  opsTwoProviders, targets: opsTargetsDefault, provider: "gh", path: "Acme/Api",
			heads: []string{opsHead1, opsHead2, opsHead64}, forget: []int64{44, 45},
		},
		{
			name: "two providers, defaults.provider gh: corp",
			hub:  opsTwoProviders, targets: opsTargetsDefault, provider: "corp", path: "acme/api",
			heads: []string{opsHead3}, forget: []int64{46},
		},
		{
			name: "two providers, defaults.provider gh: another repository",
			hub:  opsTwoProviders, targets: opsTargetsDefault, provider: "gh", path: "acme/docs",
			forget: []int64{47},
		},
		{
			name: "two providers, defaults.provider gh: a repository with no entry",
			hub:  opsTwoProviders, targets: opsTargetsDefault, provider: "gh", path: "acme/api2",
		},
		{
			// check rejects a bare path here; it never guesses a provider.
			name: "two providers, no defaults.provider: a bare path names nothing",
			hub:  opsTwoProviders, targets: opsTargetsBare, provider: "gh", path: "acme/api",
			heads: []string{opsHead1, opsHead64}, forget: []int64{44},
		},
		{
			name: "two providers, no targets.yml: a bare path names nothing",
			hub:  opsTwoProviders, provider: "corp", path: "acme/api",
			heads: []string{opsHead3}, forget: []int64{46},
		},
		{
			name: "one provider",
			hub:  opsOneProvider, targets: opsTargetsBare, provider: "gh", path: "acme/api",
			heads: []string{opsHead1, opsHead2, opsHead64}, forget: []int64{44, 45},
		},
		{
			// The implicit provider's id is its type; a prefix names another
			// provider (check rejects it).
			name: "the implicit provider",
			hub:  opsNoProviders, provider: "github", path: "acme/api",
			heads: []string{opsHead2}, forget: []int64{45},
		},
		{
			name:     "a legacy hub",
			provider: "github", path: "acme/api",
			heads: []string{opsHead2}, forget: []int64{45},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub, targets, ops := opsParse(t, c.hub, c.targets, opsFile)
			got := OpsFor(ops, hub, targets, c.provider, c.path, now)
			want := TargetOps{RecreateHeads: c.heads, Forget: c.forget, AdoptUnmarked: true}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("OpsFor =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

func TestOpsForDates(t *testing.T) {
	_, _, ops := opsParse(t, "", "", opsFile)
	for _, c := range []struct {
		now  time.Time
		want bool
	}{
		{time.Date(2026, 10, 15, 23, 59, 59, 0, time.UTC), true},
		{time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), false},
		{time.Date(2026, 10, 16, 2, 0, 0, 0, time.FixedZone("MSK", 3*60*60)), true}, // 23:00 UTC on the 15th
		{time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), true},
	} {
		if got := OpsFor(ops, nil, nil, "github", "acme/api", c.now).AdoptUnmarked; got != c.want {
			t.Errorf("AdoptUnmarked at %v = %v, want %v", c.now, got, c.want)
		}
	}
	// Recreate and forget entries have no date: they limit themselves by
	// head and PR.
	late := OpsFor(ops, nil, nil, "github", "acme/api", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(late.RecreateHeads) != 1 || len(late.Forget) != 1 || late.AdoptUnmarked {
		t.Errorf("OpsFor in 2030 = %+v", late)
	}
}

// TestOpsForBuiltInCode: operations that ParseOperations would refuse or
// warn about are handled safely.
func TestOpsForBuiltInCode(t *testing.T) {
	ops := &config.Operations{
		Recreate: []config.RecreateOp{
			{Target: "acme/api", Head: strings.ToUpper(opsHead1)},
			{Target: "ACME/api", Head: opsHead1},  // the same head again
			{Target: "acme/api", Head: ""},        // an absent branch has no head
			{Target: "acme/api", Head: "4b1d9e0"}, // abbreviated
			{Target: "acme/api", Head: opsHead1 + "0"},
			{Target: "acme/api", Head: strings.Repeat("g", 40)},
			{Target: "acme", Head: opsHead2}, // not a repository path
			{Target: "acme/api", Head: strings.ToUpper(opsHead3)},
			{Target: "acme/api", Head: opsHead64},
		},
		ForgetDeclines: []config.ForgetOp{
			{Target: "acme/api", PR: 0},
			{Target: "acme/api", PR: -3},
			{Target: "acme/api", PR: 12},
			{Target: "acme/API", PR: 12},
			{Target: "acme", PR: 13},
		},
		AdoptUnmarked: &config.UntilOp{Until: "someday"},
	}
	got := OpsFor(ops, nil, nil, "github", "acme/api", time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
	want := TargetOps{RecreateHeads: []string{opsHead1, opsHead3, opsHead64}, Forget: []int64{12}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OpsFor =\n%+v\nwant\n%+v", got, want)
	}
	if got := OpsFor(nil, nil, nil, "github", "acme/api", time.Now()); !reflect.DeepEqual(got, TargetOps{}) {
		t.Errorf("OpsFor(nil) = %+v", got)
	}
	if got := OpsFor(ops, nil, nil, "github", "", time.Now()); len(got.RecreateHeads)+len(got.Forget) != 0 {
		t.Errorf("OpsFor for an empty path = %+v", got)
	}
}

func TestOpsActiveMassClose(t *testing.T) {
	_, _, ops := opsParse(t, "", "", opsFile)
	at := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }
	got := ActiveMassClose(ops, at(2026, 9, 25, 10))
	if got == nil || *got != (config.MassCloseOp{Max: 400, Until: "2026-10-01"}) {
		t.Fatalf("ActiveMassClose = %+v, want max 400 until 2026-10-01", got)
	}
	got.Max = 1
	if ops.AllowMassClose.Max != 400 {
		t.Error("changing the result changed operations.yml")
	}
	if got := ActiveMassClose(ops, at(2026, 10, 1, 23)); got == nil {
		t.Error("allow_mass_close is not active on its until date")
	}
	if got := ActiveMassClose(ops, at(2026, 10, 2, 0)); got != nil {
		t.Errorf("allow_mass_close is active after its until date: %+v", got)
	}
	if got := ActiveMassClose(nil, at(2026, 9, 25, 10)); got != nil {
		t.Errorf("ActiveMassClose(nil) = %+v", got)
	}
	if got := ActiveMassClose(&config.Operations{}, at(2026, 9, 25, 10)); got != nil {
		t.Errorf("ActiveMassClose without the entry = %+v", got)
	}
	malformed := &config.Operations{AllowMassClose: &config.MassCloseOp{Max: 9, Until: "2026-02-30"}}
	if got := ActiveMassClose(malformed, at(2026, 1, 1, 0)); got != nil {
		t.Errorf("ActiveMassClose with a malformed date = %+v", got)
	}
}
