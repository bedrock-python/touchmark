package githube2e

import (
	"crypto/rsa"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/github"
	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// The cast of every world: placeholders, never real accounts.
const (
	// org holds the targets; the Apps are its own and installed on it.
	org = "acme"
	// outsider owns the repositories that acme's forks come from.
	outsider = "octo-org"
	// person is a member of acme with a personal access token; people's
	// writes through HTTP are not judged by the fake's journal.
	person = "alice"
	// readSlug and writeSlug are the two Apps of a hub on GitHub: the reader
	// reads (contents, pull requests and metadata read), the writer writes
	// (contents, pull requests and workflows write). Their bots are
	// "<slug>[bot]".
	readSlug  = "touchmark-read"
	writeSlug = "touchmark-write"
)

// requestTimeout bounds one request of the drivers to the fake, in place
// of httpx.DefaultTimeout: the fake answers a request with git processes
// under one lock (a GraphQL batch read of ten repositories starts about
// fifty), which can take longer than 30 s on a loaded Windows machine
// running the whole suite. A hung request still ends with go test's
// -timeout.
const requestTimeout = 10 * time.Minute

// appKey is an App's RSA key and its PEM, as GitHub hands it out.
type appKey struct {
	key *rsa.PrivateKey
	pem []byte
}

// appKeys are the reader's and the writer's keys, generated once per run:
// no key is committed.
var appKeys = sync.OnceValues(func() ([2]appKey, error) {
	var out [2]appKey
	for i := range out {
		key, pemBytes, err := ghfake.GenerateAppKey()
		if err != nil {
			return out, err
		}
		out[i] = appKey{key: key, pem: pemBytes}
	}
	return out, nil
})

// worldOptions shape a world.
type worldOptions struct {
	// flavor is the fake's product: ghfake.DotCom serves the github.com
	// layout (the API at the root) and the drivers see the provider host
	// github.com; ghfake.GHES serves /api/v3 and the drivers see the fake's
	// own host, as for any GitHub Enterprise Server.
	flavor ghfake.Flavor
	// webCommitSigning makes GHES sign the API commits of Apps (nil: the
	// fake's default, on for github.com and off for GHES).
	webCommitSigning *bool
	// writerPerms are what the writer's installation grants; nil: contents,
	// pull requests and workflows write.
	writerPerms ghfake.Permissions
	// selected installs the writer on the repositories selectRepo adds
	// only; otherwise on all of acme's.
	selected bool
}

// world is one fake GitHub with the organization acme, the person alice
// and the reader and writer Apps installed on acme.
type world struct {
	t    testing.TB
	srv  *ghfake.Server
	opts worldOptions

	readApp, writeApp   ghfake.App
	readInst, writeInst ghfake.Installation
	readKey, writeKey   []byte
	// personToken is alice's classic personal access token (repo and
	// workflow scopes); her account is personAcc.
	personToken string
	personAcc   ghfake.Account
	// selectedRepos are the names the writer's installation holds when
	// opts.selected.
	selectedRepos []string

	// rp is the provider the drivers get; host is Repo.Host.
	rp   config.ResolvedProvider
	host string
	// reg masks every secret of the world: the drivers register what they
	// mint in it, through hc.
	reg *redact.Registry
	hc  *httpx.Client
}

// newWorld starts a fake GitHub with the cast and closes it with the test.
func newWorld(t testing.TB, opts worldOptions) *world {
	t.Helper()
	if opts.flavor == "" {
		opts.flavor = ghfake.DotCom
	}
	keys, err := appKeys()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ghfake.New(ghfake.Options{Flavor: opts.flavor, WebCommitSigning: opts.webCommitSigning})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close the fake: %v", err)
		}
	})
	w := &world{t: t, srv: srv, opts: opts, readKey: keys[0].pem, writeKey: keys[1].pem, reg: redact.New()}
	w.hc = httpx.New(httpx.Options{Redact: w.reg, Timeout: requestTimeout})
	try(srv.AddOrg(org, ghfake.PlanTeam)).of(t)
	try(srv.AddOrg(outsider, ghfake.PlanTeam)).of(t)
	w.personAcc = try(srv.AddUser(person)).of(t)
	check(t, srv.Human(person))
	w.personToken = try(srv.AddPAT(person, ghfake.PATSpec{Scopes: []string{"repo", "workflow"}})).of(t)
	w.reg.Add(w.personToken, "x-access-token")
	w.readApp = try(srv.RegisterApp(ghfake.AppSpec{Slug: readSlug, Owner: org, PublicKey: &keys[0].key.PublicKey,
		Permissions: ghfake.Permissions{"contents": ghfake.Read, "pull_requests": ghfake.Read, "metadata": ghfake.Read}})).of(t)
	w.writeApp = try(srv.RegisterApp(ghfake.AppSpec{Slug: writeSlug, Owner: org, PublicKey: &keys[1].key.PublicKey,
		Permissions: ghfake.Permissions{"contents": ghfake.Write, "pull_requests": ghfake.Write, "workflows": ghfake.Write,
			"metadata": ghfake.Read}})).of(t)
	w.readInst = try(srv.Install(ghfake.InstallSpec{App: readSlug, Account: org})).of(t)
	spec := ghfake.InstallSpec{App: writeSlug, Account: org, Permissions: opts.writerPerms}
	if opts.selected {
		spec.Repos = []string{}
	}
	w.writeInst = try(srv.Install(spec)).of(t)
	for _, k := range keys {
		for _, s := range (auth.Credential{Kind: auth.App, AppKey: k.pem}).Secrets() {
			w.reg.Add(s)
		}
	}
	w.rp = w.provider("gh")
	w.host = w.rp.Host
	return w
}

// provider returns the provider id on the fake as touchmark resolves it:
// on GHES from hub.yml's url alone (the API at /api/v3). On github.com the
// provider's host is github.com, with the fake's root as the API and the
// web URL: the host is all the driver tells github.com by (its flavor,
// noreply domain and limits), and credentials go to the API's host only.
func (w *world) provider(id string) config.ResolvedProvider {
	w.t.Helper()
	if w.opts.flavor == ghfake.DotCom {
		return config.ResolvedProvider{
			Provider:   config.Provider{ID: id, Type: "github", URL: w.srv.URL(), APIURL: w.srv.APIURL(), Sign: "auto"},
			Host:       "github.com",
			APIURL:     w.srv.APIURL(),
			GraphQLURL: w.srv.GraphQLURL(),
			EnvPrefix:  config.EnvPrefix(id),
		}
	}
	hub := &config.Hub{Version: 1, ID: "conformance", Providers: []config.Provider{{ID: id, Type: "github", URL: w.srv.URL()}}}
	rps, err := hub.ResolveProviders(func(string) string { return "" })
	if err != nil || len(rps) != 1 {
		w.t.Fatalf("resolve the provider: %v", err)
	}
	return rps[0]
}

// credential is the App credential of the reader or the writer.
func (w *world) credential(app ghfake.App, key []byte) auth.Credential {
	return auth.Credential{Kind: auth.App, AppID: strconv.FormatInt(app.ID, 10), AppKey: key}
}

// drivers returns a new reader and writer over the world.
func (w *world) drivers() (platform.Reader, platform.Writer) {
	w.t.Helper()
	r, err := github.NewReader(w.rp, w.credential(w.readApp, w.readKey), w.hc)
	if err != nil {
		w.t.Fatalf("NewReader: %v", err)
	}
	wr, err := github.NewWriter(w.rp, w.credential(w.writeApp, w.writeKey), w.hc)
	if err != nil {
		w.t.Fatalf("NewWriter: %v", err)
	}
	return r, wr
}

// selectRepo adds a repository of acme to the writer's installation when
// it is installed on selected repositories.
func (w *world) selectRepo(name string) {
	w.t.Helper()
	if !w.opts.selected {
		return
	}
	w.selectedRepos = append(w.selectedRepos, name)
	check(w.t, w.srv.SetInstallationRepos(w.writeInst.ID, w.selectedRepos))
}

// violations fails the test with what the fake's journal holds.
func (w *world) violations() {
	w.t.Helper()
	if v := w.srv.Violations(); len(v) > 0 {
		w.t.Errorf("the fake saw what touchmark must never do:\n  %s", strings.Join(v, "\n  "))
	}
}

// account converts an account of the fake as the platform reports it.
func account(a ghfake.Account) platform.Account {
	out := platform.Account{ID: strconv.FormatInt(a.ID, 10), Login: a.Login, Email: a.Email}
	switch a.Type {
	case ghfake.TypeBot:
		out.Kind = platform.KindBot
	case ghfake.TypeUser:
		out.Kind = platform.KindUser
	}
	return out
}

// repo converts a repository of the fake as the platform reports it.
func (w *world) repo(r ghfake.Repo, empty bool, topics []string) platform.Repo {
	return platform.Repo{
		Host: w.host, ID: strconv.FormatInt(r.ID, 10), Path: r.FullName, DefaultBranch: r.DefaultBranch,
		Visibility: r.Visibility, ObjectFormat: r.ObjectFormat, Archived: r.Archived, Fork: r.Fork, Empty: empty,
		Topics: topics,
	}
}

// result is a value and an error, for try(f()).of(t).
type result[T any] struct {
	v   T
	err error
}

// try pairs the results of a call.
func try[T any](v T, err error) result[T] { return result[T]{v: v, err: err} }

// of returns the value, failing the test on the error.
func (r result[T]) of(t testing.TB) T {
	t.Helper()
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.v
}

// check fails the test on err.
func check(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// heavy skips a heavy test outside Linux unless TOUCHMARK_HEAVY_TESTS is
// set: the fake starts a git process for most requests, which is slow on
// Windows.
func heavy(t testing.TB) {
	t.Helper()
	if runtime.GOOS != "linux" && os.Getenv("TOUCHMARK_HEAVY_TESTS") == "" {
		t.Skipf("a heavy test: it runs on Linux (set TOUCHMARK_HEAVY_TESTS=1 to run it on %s)", runtime.GOOS)
	}
}
