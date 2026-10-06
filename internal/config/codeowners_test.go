package config

import (
	"strings"
	"testing"
)

// TestCheckCodeowners: a hub whose CODEOWNERS still names the template's
// placeholder owner on a rule line gets a warning per file; comments, other
// owners and the template itself (its placeholder id) get none.
func TestCheckCodeowners(t *testing.T) {
	t.Parallel()
	const template = "# Replace @acme/hub-maintainers with your team.\n/packs/  @acme/hub-maintainers\n# /packs/x/  @acme/hub-maintainers\n"
	const own = "# Replace @acme/hub-maintainers with your team.\n/packs/  @octo-org/maintainers # was @acme/hub-maintainers\n"
	for _, tc := range []struct {
		name  string
		hub   *Hub
		files map[string]string
		want  []string
	}{
		{name: "placeholder", hub: &Hub{ID: "acme-eng"},
			files: map[string]string{".github/CODEOWNERS": template, ".gitlab/CODEOWNERS": "/hub.yml @ACME/Hub-Maintainers @octo-org/x\n", ".gitea/CODEOWNERS": own},
			want:  []string{".github/CODEOWNERS: names @acme/hub-maintainers", ".gitlab/CODEOWNERS: names @acme/hub-maintainers"}},
		{name: "own team", hub: &Hub{ID: "acme-eng"}, files: map[string]string{".github/CODEOWNERS": own, "CODEOWNERS": "* @octo-org/hub-maintainers-two\n"}},
		{name: "the template", hub: &Hub{ID: PlaceholderID}, files: map[string]string{".github/CODEOWNERS": template}},
		{name: "a legacy hub", hub: &Hub{Legacy: true}, files: map[string]string{"CODEOWNERS": template}},
		{name: "no files", hub: &Hub{ID: "acme-eng"}},
	} {
		files := map[string][]byte{}
		for k, v := range tc.files {
			files[k] = []byte(v)
		}
		got := CheckCodeowners(tc.hub, files)
		if len(got) != len(tc.want) {
			t.Errorf("%s: warnings %v, want %d", tc.name, got, len(tc.want))
			continue
		}
		for i, want := range tc.want {
			if !strings.HasPrefix(got[i].String(), want) {
				t.Errorf("%s: warning %q, want %q…", tc.name, got[i], want)
			}
		}
	}
}
