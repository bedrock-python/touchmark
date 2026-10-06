package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/docsurl"
)

func probeFlags() (*flagSet, *options) {
	return newFlagSet("probe")
}

// runProbe is the isolation probe: run in a job that any
// branch of the hub can start (a GitLab merge request pipeline), it exits 2
// when that job sees a write credential or a signing key, naming the
// variables and never their values, and 0 otherwise.
func runProbe(_ context.Context, e *env, _ *options) error {
	res := distribute.Probe(e.getenv, e.environ)
	if len(res.Exposed) == 0 {
		_, err := fmt.Fprintln(e.stdout, "touchmark probe: no write credential or signing key is visible in this job")
		return err
	}
	verb := "is"
	if len(res.Exposed) > 1 {
		verb = "are"
	}
	return configErrorf("%s %s visible in this job, which any branch of the hub can run: keep write credentials and signing keys "+
		"in the protected environment of the distribute job; see "+docsurl.WriteIsolation, strings.Join(res.Exposed, ", "), verb)
}
