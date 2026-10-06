package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// offlineWarning is the report's warning of an offline plan.
const offlineWarning = "offline plan: this hub pull request has no read credential (pull requests from Dependabot and from forks get no secrets), " +
	"so no target was read: the report shows the hub's side only (the packs it changes, check, operations). " +
	"Run plan with the read credential before you merge it"

// errOffline is what every call of an offline provider returns.
var errOffline = errors.New("offline: this hub pull request has no read credential")

// offlinePlan reports whether plan runs offline: in a
// hub pull request in which no provider has the secret part of a read
// credential (…READ_TOKEN, …READ_APP_KEY). A pull request from Dependabot
// or from a fork of a public hub gets no Actions secrets, while variables
// such as …READ_APP_ID may still be set. Anywhere else, and in a pull
// request in which any provider has one, a missing read credential stays
// the configuration error readCredentials reports.
func offlinePlan(hctx hubch.Context, pps []planProvider, getenv func(string) string) bool {
	if !pullRequestRun(hctx) || len(pps) == 0 {
		return false
	}
	for _, pp := range pps {
		prefixes := []string{pp.EnvPrefix}
		if pp.Short {
			prefixes = append(prefixes, shortEnvPrefix)
		}
		for _, prefix := range prefixes {
			for _, suffix := range []string{"READ_TOKEN", "READ_APP_KEY"} {
				if strings.TrimSpace(getenv(prefix+suffix)) != "" {
					return false
				}
			}
		}
	}
	return true
}

// offlineReader is the reader of every provider of an offline plan: it
// reaches nothing, and each call fails with errOffline, which the run
// records as a provider not checked without credentials (its resolve
// incomplete, no sweep).
type offlineReader struct{}

var _ platform.Reader = offlineReader{}

func (offlineReader) Probe(context.Context) (platform.Caps, error) {
	return platform.Caps{}, errOffline
}

func (offlineReader) Self(context.Context) (platform.Account, error) {
	return platform.Account{}, errOffline
}

func (offlineReader) Lookup(context.Context, string) (platform.Account, error) {
	return platform.Account{}, errOffline
}

func (offlineReader) Resolve(context.Context, platform.Selector) (platform.Resolved, error) {
	return platform.Resolved{}, errOffline
}

func (offlineReader) Repo(context.Context, string) (platform.Repo, error) {
	return platform.Repo{}, errOffline
}

func (offlineReader) ReadFile(context.Context, platform.Repo, string, string, int64) (platform.File, error) {
	return platform.File{}, errOffline
}

func (offlineReader) Remote(context.Context, platform.Repo) (platform.Remote, error) {
	return platform.Remote{}, errOffline
}

func (offlineReader) PRs(context.Context, platform.Repo, []string, []platform.Account) ([]platform.PR, error) {
	return nil, errOffline
}

func (offlineReader) OpenPRsBy(context.Context, []platform.Account, []string) (platform.Swept, error) {
	return platform.Swept{}, errOffline
}
