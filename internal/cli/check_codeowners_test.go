package cli

import (
	"strings"
	"testing"
)

// check warns while a CODEOWNERS file of the hub names the template's
// placeholder owner, and passes clean once it names the hub's team.
func TestCheckCodeowners(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write(".github/CODEOWNERS", "# Replace @acme/hub-maintainers with your team.\n/packs/  @acme/hub-maintainers\n")
	h.commit("codeowners")
	s := newScenario(t, "", h, nil)
	want := ".github/CODEOWNERS: names @acme/hub-maintainers, the template's placeholder"
	if res := s.run(0, "check"); !strings.Contains(res.stdout, want) {
		t.Errorf("check: stdout %q, want %q", res.stdout, want)
	}
	h.write(".github/CODEOWNERS", "/packs/  @octo-org/hub-maintainers\n")
	h.commit("our team")
	if res := s.run(0, "check"); strings.Contains(res.stdout, "CODEOWNERS") {
		t.Errorf("check: stdout %q", res.stdout)
	}
}
