package gitx

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPushLease(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	ids := s.chain("main", "", 3)
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 3); err != nil {
		t.Fatal(err)
	}
	branch := func() string {
		out, err := s.g.Run(t.Context(), nil, "rev-parse", "--verify", "--quiet", "refs/heads/touchmark/hub")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	steps := []struct {
		name       string
		spec       PushSpec
		want       PushStatus
		wantBranch string
	}{
		{"create", PushSpec{Commit: ids[0], Expect: ""}, PushOK, ids[0]},
		// Git checks "up to date" before the lease.
		{"same commit", PushSpec{Commit: ids[0], Expect: ids[2]}, PushUpToDate, ids[0]},
		{"create again", PushSpec{Commit: ids[1], Expect: ""}, PushStale, ids[0]},
		{"wrong lease", PushSpec{Commit: ids[1], Expect: ids[2]}, PushStale, ids[0]},
		{"move", PushSpec{Commit: ids[1], Expect: ids[0]}, PushOK, ids[1]},
		// Backwards is a forced update: the lease allows it.
		{"force back", PushSpec{Commit: ids[0], Expect: ids[1]}, PushOK, ids[0]},
		{"delete stale", PushSpec{Commit: "", Expect: ids[1]}, PushStale, ids[0]},
		{"delete", PushSpec{Commit: "", Expect: ids[0]}, PushOK, ""},
		{"delete gone", PushSpec{Commit: "", Expect: ids[0]}, PushStale, ""},
	}
	for _, st := range steps {
		st.spec.Branch = "touchmark/hub"
		res, err := tr.Push(t.Context(), st.spec)
		if err != nil || res.Status != st.want {
			t.Fatalf("%s: Push(%+v) = %+v, %v; want %v", st.name, st.spec, res, err, st.want)
		}
		if got := branch(); got != st.wantBranch {
			t.Fatalf("%s: branch at %q, want %q", st.name, got, st.wantBranch)
		}
		if res.Message == "" {
			t.Errorf("%s: empty message", st.name)
		}
	}
	for _, bad := range []PushSpec{
		{Branch: "x"},
		{Branch: "", Commit: ids[0]},
		{Branch: "-f", Commit: ids[0]},
		{Branch: "x", Commit: "main"},
		{Branch: "x", Commit: ids[0], Expect: "HEAD"},
		{Branch: "refs/heads/x", Commit: ids[0]},
	} {
		if res, err := tr.Push(t.Context(), bad); err == nil {
			t.Errorf("Push(%+v) = %+v, want an error", bad, res)
		}
	}
	// A commit the repository does not have is refused by git itself.
	if res, err := tr.Push(t.Context(), PushSpec{Branch: "y", Commit: missingOID}); err != nil || res.Status != PushError {
		t.Errorf("Push(unknown commit) = %+v, %v", res, err)
	}
}

// TestPushHiddenRef: a hidden ref under refs/touchmark/ (the stage ref of an
// API commit) moves with a lease like a branch, and no ref outside it can
// be pushed that way.
func TestPushHiddenRef(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	ids := s.chain("main", "", 2)
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 2); err != nil {
		t.Fatal(err)
	}
	const stage = HiddenRefPrefix + "0123456789abcdef/stage"
	for _, st := range []struct {
		name string
		spec PushSpec
		want PushStatus
	}{
		{"create", PushSpec{Ref: stage, Commit: ids[0]}, PushOK},
		{"create again", PushSpec{Ref: stage, Commit: ids[1]}, PushStale},
		{"replace", PushSpec{Ref: stage, Commit: ids[1], Expect: ids[0]}, PushOK},
		{"delete", PushSpec{Ref: stage, Expect: ids[1]}, PushOK},
	} {
		res, err := tr.Push(t.Context(), st.spec)
		if err != nil || res.Status != st.want {
			t.Fatalf("%s: Push(%+v) = %+v, %v; want %v", st.name, st.spec, res, err, st.want)
		}
	}
	if refs, err := tr.RemoteRefs(t.Context(), stage); err != nil || len(refs) != 0 {
		t.Errorf("after the deletion: %v, %v", refs, err)
	}
	for _, ref := range []string{"refs/heads/main", "refs/tags/v1", HiddenRefPrefix, HiddenRefPrefix + "x/../y", "refs/touchmark"} {
		if res, err := tr.Push(t.Context(), PushSpec{Ref: ref, Commit: ids[0]}); err == nil {
			t.Errorf("Push to %q = %+v, want an error", ref, res)
		}
	}
}

// TestPushPolicy: server-side hooks refuse pushes the way platforms do; the
// client's own hooks never run.
func TestPushPolicy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cases := []struct {
		name   string
		stderr string
		want   PushStatus
		in     string
	}{
		{"gitlab protected", "GitLab: You are not allowed to force push code to a protected branch on this project.", PushPolicy, "force push"},
		{"gitlab unsigned", "GitLab: Commit must be signed", PushUnsigned, "must be signed"},
		{"github rulesets", "error: GH013: Repository rule violations found for refs/heads/touchmark/hub.\n- Cannot update this protected ref.", PushPolicy, "GH013"},
		{"github signatures", "error: GH013: Repository rule violations found for refs/heads/touchmark/hub.\n- Commits must have verified signatures.", PushUnsigned, "verified signatures"},
		{"gitea unverified", "Gitea: Branch touchmark/hub is protected from unverified commit 0123abc", PushUnsigned, "unverified"},
		{"plain hook", "nope", PushPolicy, "pre-receive hook declined"},
	}
	for i, c := range cases {
		s := newServed(t, root, "repo"+string(rune('a'+i))+".git")
		head := s.chain("main", "", 1)[0]
		var script strings.Builder
		script.WriteString("#!/bin/sh\n")
		for _, line := range strings.Split(c.stderr, "\n") {
			script.WriteString("echo '" + line + "' >&2\n")
		}
		script.WriteString("exit 1\n")
		s.hook("pre-receive", script.String())
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tr := newTarget(t, s.dir, Auth{})
			if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
				t.Fatal(err)
			}
			res, err := tr.Push(t.Context(), PushSpec{Branch: "touchmark/hub", Commit: head})
			if err != nil || res.Status != c.want || !strings.Contains(res.Message, c.in) {
				t.Errorf("Push() = %+v, %v; want %v with %q", res, err, c.want, c.in)
			}
			if !strings.Contains(res.Message, "pre-receive hook declined") {
				t.Errorf("message %q lacks the porcelain reason", res.Message)
			}
		})
	}
}

// Outputs of git push, with the status ParsePush gives them. Remote
// messages are those of the platforms' documentation and sources (see
// ParsePush).
var pushOutputs = []struct {
	name, stdout, stderr string
	want                 PushStatus
	in                   string
}{
	{"new branch", "To https://example.com/r.git\n*\t" + oidA + ":refs/heads/b\t[new branch]\nDone\n", "", PushOK, "[new branch]"},
	{"fast forward", "To x\n \t" + oidA + ":refs/heads/b\t" + oidA[:7] + ".." + oidB[:7] + "\nDone\n", "", PushOK, ".."},
	{"forced", "To x\n+\t" + oidA + ":refs/heads/b\t" + oidA[:7] + "..." + oidB[:7] + " (forced update)\nDone\n", "", PushOK, "forced update"},
	{"deleted", "To x\n-\t:refs/heads/b\t[deleted]\nDone\n", "", PushOK, "[deleted]"},
	{"up to date", "To x\n=\t" + oidA + ":refs/heads/b\t[up to date]\nDone\n", "", PushUpToDate, "up to date"},
	{"crlf", "To x\r\n=\t" + oidA + ":refs/heads/b\t[up to date]\r\nDone\r\n", "", PushUpToDate, "up to date"},
	{"stale", "To x\n!\t" + oidA + ":refs/heads/b\t[rejected] (stale info)\nDone\n",
		"error: failed to push some refs to 'x'\n", PushStale, "stale info"},
	{"stale delete", "To x\n!\t(delete):refs/heads/b\t[rejected] (stale info)\nDone\n",
		"error: atomic push failed for ref refs/heads/b. status: 7\nfatal: the remote end hung up unexpectedly\n", PushStale, "stale info"},
	{"github protected", "To x\n!\t" + oidA + ":refs/heads/main\t[remote rejected] (protected branch hook declined)\nDone\n",
		"remote: error: GH006: Protected branch update failed for refs/heads/main.\nremote: error: Changes must be made through a pull request.\n", PushPolicy, "GH006"},
	{"github ruleset", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (push declined due to repository rule violations)\nDone\n",
		"remote: error: GH013: Repository rule violations found for refs/heads/b.\nremote: Review all repository rules at https://github.com/o/r/rules?ref=refs%2Fheads%2Fb\nremote: \nremote: - Cannot create ref due to creations being restricted.\nremote: \n",
		PushPolicy, "creations being restricted"},
	{"github signatures", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (push declined due to repository rule violations)\nDone\n",
		"remote: error: GH013: Repository rule violations found for refs/heads/b.\nremote: - Commits must have verified signatures.\nremote:   Found 1 violation:\nremote:   " + oidA + "\n",
		PushUnsigned, "verified signatures"},
	{"github classic signatures", "To x\n!\t" + oidA + ":refs/heads/main\t[remote rejected] (protected branch hook declined)\nDone\n",
		"remote: error: GH006: Protected branch update failed for refs/heads/main.\nremote: error: Commits must have verified signatures.\n", PushUnsigned, "verified signatures"},
	{"github app workflows", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission)\nDone\n",
		"", PushWorkflows, "workflows"},
	{"oauth workflow scope", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (refusing to allow an OAuth App to create or update workflow `.github/workflows/ci.yml` without `workflow` scope)\nDone\n",
		"", PushWorkflows, "workflow"},
	{"pat workflow scope", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without `workflow` scope)\nDone\n",
		"", PushWorkflows, "Personal Access Token"},
	{"gitlab unsigned", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (pre-receive hook declined)\nDone\n",
		"remote: GitLab: Commit must be signed\n", PushUnsigned, "Commit must be signed"},
	{"gitlab unsigned old", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (pre-receive hook declined)\nDone\n",
		"remote: GitLab: Commit must be signed with a GPG key\n", PushUnsigned, "GPG key"},
	{"gitlab protected", "To x\n!\t" + oidA + ":refs/heads/main\t[remote rejected] (pre-receive hook declined)\nDone\n",
		"remote: GitLab: You are not allowed to push code to protected branches on this project.\n", PushPolicy, "protected branches"},
	{"gitea unverified", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (pre-receive hook declined)\nDone\n",
		"remote: \nremote: Gitea: Branch b is protected from unverified commit " + oidA + "\nremote: \n", PushUnsigned, "unverified"},
	// Gitea 1.26 and 1.27 fail their own check of a branch that requires
	// signed commits and refuse the unsigned push with a 500 of the
	// pre-receive hook (seen live by the e2e fact "signed-commits"): a
	// policy refusal, never a transient error.
	{"gitea unsigned 1.26 and 1.27", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (pre-receive hook declined)\nDone\n",
		"remote: error: Internal Server Error (no message for end users)\nTo x\n ! [remote rejected] " + oidA + " -> b (pre-receive hook declined)\n",
		PushPolicy, "pre-receive hook declined"},
	{"forgejo force", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (pre-receive hook declined)\nDone\n",
		"remote: Forgejo: branch b is protected from force push\n", PushPolicy, "force push"},
	{"deletion prohibited", "To x\n!\t:refs/heads/b\t[remote rejected] (deletion prohibited)\nDone\n", "", PushPolicy, "deletion prohibited"},
	{"unknown remote reason", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (failed to update ref)\nDone\n", "", PushError, "failed to update ref"},
	{"unpacker", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (unpacker error)\n", "error: remote unpack failed: eof before pack header was fully read\n", PushError, "unpacker error"},
	{"remote failure", "To x\n!\t" + oidA + ":refs/heads/b\t[remote failure] (remote failed to report status)\n", "", PushError, "remote failure"},
	{"non fast forward", "To x\n!\t" + oidA + ":refs/heads/b\t[rejected] (non-fast-forward)\n", "", PushError, "non-fast-forward"},
	{"atomic sibling", "To x\n!\t" + oidA + ":refs/heads/a\t[rejected] (atomic push failed)\n!\t" + oidB + ":refs/heads/b\t[rejected] (stale info)\n", "", PushStale, "stale info"},
	{"401 prompt", "", "fatal: could not read Username for 'https://github.com': terminal prompts disabled\n", PushAuth, "could not read Username"},
	{"401", "", "remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/o/r.git/'\n", PushAuth, "Authentication failed"},
	{"401 status", "", "fatal: unable to access 'https://gitea.example/o/r.git/': The requested URL returned error: 401\n", PushAuth, "401"},
	// A 403 is the identity's permission on this repository, not a broken
	// credential: it must not count towards provider-down.
	{"403", "", "remote: Permission to o/r.git denied to acme-bot[bot].\nfatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 403\n", PushPermission, "403"},
	{"gitlab 403", "", "remote: You are not allowed to push code to this project.\nfatal: unable to access 'https://gitlab.com/g/p.git/': The requested URL returned error: 403\n", PushPermission, "403"},
	{"github permission", "", "remote: Permission to o/r.git denied to acme-bot[bot].\nfatal: the remote end hung up unexpectedly\n", PushPermission, "Permission to"},
	{"429", "", "fatal: unable to access 'https://codeberg.org/o/r.git/': The requested URL returned error: 429\n", PushRateLimited, "429"},
	{"429 rpc", "", "error: RPC failed; HTTP 429 curl 22 The requested URL returned error: 429\nsend-pack: unexpected disconnect while reading sideband packet\nfatal: the remote end hung up unexpectedly\n", PushRateLimited, "429"},
	{"413", "", "error: RPC failed; HTTP 413 curl 22 The requested URL returned error: 413\nsend-pack: unexpected disconnect while reading sideband packet\nfatal: the remote end hung up unexpectedly\n", PushError, "413"},
	{"400 rpc", "", "error: RPC failed; HTTP 400 curl 92 HTTP/2 stream 5 was not closed cleanly: CANCEL (err 8)\nfatal: the remote end hung up unexpectedly\n", PushError, "400"},
	{"502", "", "error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502\nfatal: the remote end hung up unexpectedly\n", PushError, "502"},
	{"408", "", "error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: the remote end hung up unexpectedly\n", PushError, "408"},
	{"gitlab basic", "", "remote: HTTP Basic: Access denied. The provided password or token is incorrect.\nfatal: Authentication failed for 'https://gitlab.com/g/p.git/'\n", PushAuth, "Access denied"},
	{"not found", "", "remote: Repository not found.\nfatal: repository 'https://github.com/o/r.git/' not found\n", PushError, "not found"},
	{"network", "", "fatal: unable to access 'https://example.com/r.git/': Could not resolve host: example.com\n", PushError, "resolve host"},
	{"nothing", "", "", PushError, "no status"},
	{"escapes", "To x\n!\t" + oidA + ":refs/heads/b\t[remote rejected] (pre-receive hook declined)\n",
		"remote: \x1b[31mGitLab: You are not allowed to push code to protected branches on this project.\x1b[0m\r\n", PushPolicy, "protected branches"},
}

func TestParsePush(t *testing.T) {
	t.Parallel()
	for _, c := range pushOutputs {
		got := ParsePush(c.stdout, c.stderr)
		if got.Status != c.want || !strings.Contains(got.Message, c.in) {
			t.Errorf("%s: ParsePush() = %+v, want %v with %q", c.name, got, c.want, c.in)
		}
		checkPushResult(t, c.stdout, c.stderr, got)
	}
	// A long remote message is cut; escapes are dropped.
	long := ParsePush("!\t"+oidA+":refs/heads/b\t[remote rejected] (pre-receive hook declined)\n",
		strings.Repeat("remote: GitLab: protected branch "+strings.Repeat("é", 50)+"\n", 100))
	if long.Status != PushPolicy || len(long.Message) > maxPushMessage || !utf8.ValidString(long.Message) {
		t.Errorf("ParsePush(long) = %v, %d bytes, valid %v", long.Status, len(long.Message), utf8.ValidString(long.Message))
	}
	if esc := ParsePush("", "fatal: \x1b]0;title\x07bad\n"); strings.ContainsAny(esc.Message, "\x1b\x07") {
		t.Errorf("ParsePush kept control characters: %q", esc.Message)
	}
}

func TestPushResultTransient(t *testing.T) {
	t.Parallel()
	// The outcome of these is unknown: the push may have been applied, and
	// the server's answer lost ("remote failure": applied, without a
	// status report).
	transient := map[string]bool{"network": true, "remote failure": true, "502": true, "408": true}
	for _, c := range pushOutputs {
		got := ParsePush(c.stdout, c.stderr).Transient()
		if want := transient[c.name]; got != want {
			t.Errorf("%s: Transient() = %v, want %v", c.name, got, want)
		}
	}
	// A push that ran into its own bound, as Push reports it.
	timedOut := PushResult{Status: PushError, Message: "git push timed out after 3m0s: whether refs/heads/b moved is unknown"}
	if !timedOut.Transient() {
		t.Error("a timed-out push is not transient")
	}
	gone := ParsePush("", "fatal: unable to access 'http://127.0.0.1:1/r.git/': Failed to connect to 127.0.0.1 port 1 after 0 ms: Could not connect to server\n")
	if gone.Status != PushError || !gone.Transient() {
		t.Errorf("ParsePush(server gone) = %+v, transient %v", gone, gone.Transient())
	}
	for _, s := range []PushStatus{PushAuth, PushPermission, PushRateLimited, PushStale} {
		if (PushResult{Status: s, Message: "The requested URL returned error: 503"}).Transient() {
			t.Errorf("a %s result is transient", s)
		}
	}
}

func TestPushStatusString(t *testing.T) {
	t.Parallel()
	for s, want := range map[PushStatus]string{
		PushOK: "ok", PushUpToDate: "up-to-date", PushStale: "stale", PushPolicy: "policy", PushWorkflows: "workflows",
		PushUnsigned: "unsigned", PushAuth: "auth", PushError: "error", PushPermission: "permission",
		PushRateLimited: "rate-limited", 0: "unknown", 200: "unknown",
	} {
		if got := s.String(); got != want {
			t.Errorf("PushStatus(%d).String() = %q, want %q", s, got, want)
		}
	}
}

// checkPushResult asserts what ParsePush promises for any input.
func checkPushResult(t *testing.T, stdout, stderr string, got PushResult) {
	t.Helper()
	if got.Status < PushOK || got.Status > PushRateLimited {
		t.Fatalf("ParsePush(%q, %q): status %d out of range", stdout, stderr, got.Status)
	}
	if len(got.Message) > maxPushMessage || !utf8.ValidString(got.Message) {
		t.Fatalf("ParsePush(%q, %q): message of %d bytes, valid UTF-8 %v", stdout, stderr, len(got.Message), utf8.ValidString(got.Message))
	}
	for _, r := range got.Message {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r < 0xa0) {
			t.Fatalf("ParsePush(%q, %q): control character %U in %q", stdout, stderr, r, got.Message)
		}
	}
	if again := ParsePush(stdout, stderr); again != got {
		t.Fatalf("ParsePush(%q, %q) is not deterministic: %+v then %+v", stdout, stderr, got, again)
	}
}

func FuzzParsePush(f *testing.F) {
	for _, c := range pushOutputs {
		f.Add(c.stdout, c.stderr)
	}
	f.Add("!\t:\t (", "remote:")
	f.Add("!\tx:y\t[remote rejected] ()", "")
	f.Add("=\t:\t", "\xff\xfe")
	f.Fuzz(func(t *testing.T, stdout, stderr string) {
		checkPushResult(t, stdout, stderr, ParsePush(stdout, stderr))
	})
}
