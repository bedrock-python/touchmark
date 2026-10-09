package distribute

import (
	"errors"
	"testing"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
)

// TestKeptBody: a body touchmark writes back is, on every platform that
// keeps the marker in the body, byte for byte what it always was: people's
// text made inert, the new marker last. Where the marker lives apart (Azure
// DevOps), a description touchmark leaves as read stays as read, people's
// text and all, and only its marker line is checked as touchmark's text; a
// description touchmark changes (a control ticked off) is made inert as
// anywhere.
func TestKeptBody(t *testing.T) {
	t.Parallel()
	const (
		oldLine = "<!-- touchmark:v1 hub=acme fp=github.com/1 stream=sync key=sha256:aa data=x -->"
		newLine = "<!-- touchmark:v1 hub=acme fp=github.com/1 stream=sync key=sha256:bb data=y -->"
	)
	read := "Sync.\n\n- [x] <!-- touchmark:recreate --> Rebuild the branch\n/label ~smuggled\ncc @bob\n\n" + oldLine
	unticked := prbody.Untick(read, prbody.ControlRecreate)
	if unticked == read {
		t.Fatal("fixture: Untick changed nothing")
	}
	for _, f := range []fake.Flavor{fake.GitHub, fake.GitLab, fake.Gitea, fake.Forgejo, fake.Bitbucket, fake.AzureDevOps} {
		x := &targetExec{t: &target{prov: &provider{caps: fake.CapsFor(f)}}}
		for _, body := range []string{read, unticked} {
			got, kept := x.keptBody(read, body, newLine)
			inert := prbody.ReplaceMarker(prbody.Inert(marker.Strip(body)), newLine)
			if f != fake.AzureDevOps || body != read {
				if got != inert || kept {
					t.Errorf("%s: keptBody = %q (kept %v), want %q", f, got, kept, inert)
				}
				continue
			}
			desc, line := marker.Detach(got)
			if !kept || desc != marker.Strip(read) || line != newLine {
				t.Errorf("%s: keptBody = %q (kept %v): the description must stay as read", f, got, kept)
			}
			act := writeAct{kind: actEdit, pr: 1, edit: platform.PREdit{Body: &got}, kept: kept}
			if err := unsafeText([]writeAct{act}); err != nil {
				t.Errorf("%s: a kept description is checked as touchmark's text: %v", f, err)
			}
			act.kept = false
			if err := unsafeText([]writeAct{act}); !errors.Is(err, prbody.ErrUnsafe) {
				t.Errorf("%s: people's text sent as is passes the check: %v", f, err)
			}
		}
	}
}
