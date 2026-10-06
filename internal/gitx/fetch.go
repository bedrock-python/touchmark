package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// remoteRefPrefix is where FetchBranch keeps the branches it fetched.
const remoteRefPrefix = "refs/touchmark/remote/"

// RemoteRefs lists refs on the remote (git ls-remote --refs origin <refs…>):
// full ref name → commit id. Missing refs are absent from the map.
//
// Each ref must be a full name ("refs/heads/main"); only exact matches are
// kept (ls-remote patterns also match refs that merely end in one). With no
// refs every ref is listed. The object id is the one the ref points to, not
// peeled.
func (t *TargetRepo) RemoteRefs(ctx context.Context, refs ...string) (map[string]string, error) {
	want := make(map[string]bool, len(refs))
	for _, r := range refs {
		if !strings.HasPrefix(r, "refs/") {
			return nil, fmt.Errorf("git ls-remote: %q is not a full ref name", abbrev(r))
		}
		if err := checkRefName(r); err != nil {
			return nil, fmt.Errorf("git ls-remote: %w", err)
		}
		want[r] = true
	}
	out, err := t.run(ctx, cmdOpts{network: true}, append([]string{"ls-remote", "--refs", remoteName}, refs...)...)
	if err != nil {
		return nil, err
	}
	got := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		oid, ref, ok := strings.Cut(line, "\t")
		if !ok || !isOID(oid) || ref == "" {
			return nil, fmt.Errorf("git ls-remote: unexpected line %q", abbrev(line))
		}
		if len(refs) == 0 || want[ref] {
			got[ref] = oid
		}
	}
	return got, nil
}

// FetchBranch fetches refs/heads/<branch> into refs/touchmark/remote/<branch>
// with --filter=blob:none --depth=<depth> --no-tags, and returns its commit
// id. It returns ("", false, nil) when the branch does not exist remotely.
//
// --depth counts from the fetched tip and may shorten history fetched
// before: fetching the base again with depth 1 after DeepenSince cuts it
// back to one commit (git records the tip as shallow). The branch name
// must be valid (git check-ref-format); depth must be positive.
func (t *TargetRepo) FetchBranch(ctx context.Context, branch string, depth int) (sha string, ok bool, err error) {
	if err := checkBranch(branch); err != nil {
		return "", false, fmt.Errorf("git fetch: %w", err)
	}
	if depth < 1 {
		return "", false, fmt.Errorf("git fetch: depth %d is not positive", depth)
	}
	local := remoteRefPrefix + branch
	unlock, err := t.lockFetch()
	if err != nil {
		return "", false, err
	}
	defer unlock()
	_, err = t.run(ctx, cmdOpts{network: true}, "fetch", "--quiet",
		"--filter=blob:none", "--depth="+strconv.Itoa(depth), "--no-tags",
		"--no-write-fetch-head", "--recurse-submodules=no",
		remoteName, "+refs/heads/"+branch+":"+local)
	if err != nil {
		if isMissingRemoteRef(err) {
			return "", false, nil
		}
		return "", false, err
	}
	sha, err = t.resolve(ctx, local+"^{commit}")
	if err != nil {
		return "", false, err
	}
	return sha, true, nil
}

// isMissingRemoteRef reports whether err is git fetch saying that the
// remote has no such ref ("fatal: couldn't find remote ref <ref>", the
// wording of remote.c since git 1.x; LC_ALL=C keeps it English).
func isMissingRemoteRef(err error) bool {
	var e *Error
	return errors.As(err, &e) && strings.Contains(e.Stderr, "couldn't find remote ref")
}

// DeepenSince fetches more of branch's history, back to since
// (--shallow-since), without blobs. It is used to prove that a merge's
// second parent is an ancestor of the base.
//
// The fetch updates no ref: refs/touchmark/remote/<branch> keeps the head
// FetchBranch saw, and the history fetched is that of the remote's current
// head (the same one, unless the branch moved meanwhile). Commits older
// than since become the new shallow boundary; history fetched before
// through other refs may be cut there. since is sent as its Unix time.
// A server without deepen-since support fails with an *Error: the caller
// falls back to the platform's compare API.
func (t *TargetRepo) DeepenSince(ctx context.Context, branch string, since time.Time) error {
	if err := checkBranch(branch); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	if since.IsZero() || since.Unix() <= 0 {
		return fmt.Errorf("git fetch: invalid --shallow-since date %v", since)
	}
	unlock, err := t.lockFetch()
	if err != nil {
		return err
	}
	defer unlock()
	_, err = t.run(ctx, cmdOpts{network: true}, "fetch", "--quiet",
		"--filter=blob:none", "--shallow-since=@"+strconv.FormatInt(since.Unix(), 10)+" +0000",
		"--no-tags", "--no-write-fetch-head", "--recurse-submodules=no",
		remoteName, "refs/heads/"+branch)
	if isMissingRemoteRef(err) {
		return fmt.Errorf("git fetch: branch %s no longer exists: %w", branch, ErrNotFound)
	}
	return err
}

// FetchBlobs fetches blobs by id the way git's promisor fetch does:
// "-c fetch.negotiationAlgorithm=noop fetch origin --no-tags
// --no-write-fetch-head --recurse-submodules=no --filter=blob:none --stdin"
// with the ids on stdin. Used for .gitattributes files.
//
// The command also passes --quiet. The noop negotiation sends no "have"
// lines, so the server cannot send the blobs as deltas against objects it
// believes the client has (which a blobless repository lacks). The remote
// must allow fetching objects by id (uploadpack.allowAnySHA1InWant or
// allowReachableSHA1InWant; GitHub, GitLab, Gitea and Forgejo do for
// partial clones). Every id must be a full object id; duplicates are
// fetched once, and ids already present are not asked for. It fails when a
// blob is still missing afterwards.
func (t *TargetRepo) FetchBlobs(ctx context.Context, oids []string) error {
	var ids []string
	seen := make(map[string]bool, len(oids))
	for _, id := range oids {
		if !isOID(id) {
			return fmt.Errorf("git fetch: invalid object id %q", abbrev(id))
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	missing, err := t.missing(ctx, ids)
	if err != nil || len(missing) == 0 {
		return err
	}
	var in bytes.Buffer
	for _, id := range missing {
		in.WriteString(id)
		in.WriteByte('\n')
	}
	unlock, err := t.lockFetch()
	if err != nil {
		return err
	}
	_, err = t.run(ctx, cmdOpts{network: true, stdin: &in},
		"-c", "fetch.negotiationAlgorithm=noop",
		"fetch", "--quiet", remoteName, "--no-tags", "--no-write-fetch-head",
		"--recurse-submodules=no", "--filter=blob:none", "--stdin")
	unlock()
	if err != nil {
		return err
	}
	still, err := t.missing(ctx, missing)
	if err != nil {
		return err
	}
	if len(still) > 0 {
		return fmt.Errorf("git fetch: %d of %d blobs still missing, %s the first: %w", len(still), len(missing), still[0], ErrNotFound)
	}
	return nil
}

// Bounds of the objects of a target read into memory: anyone who may push
// to a target writes them, and one object far larger than these (a commit
// message of gigabytes compresses to a few megabytes of pack) would
// exhaust the memory of the whole run.
const (
	// MaxCommitObject bounds a commit object FirstParentLog and Commit read:
	// its headers and message.
	MaxCommitObject = 1 << 20
	// MaxReadBlob bounds a blob ReadBlob returns.
	MaxReadBlob = 8 << 20
)

// ErrTooLarge is wrapped by ReadBlob, Commit and FirstParentLog when an
// object exceeds MaxReadBlob or MaxCommitObject. The object's size is read
// first (cat-file --batch-check), so nothing of it is loaded.
var ErrTooLarge = errors.New("object too large")

// ReadBlob returns a blob that is present locally (never fetches). A
// missing blob wraps ErrNotFound; an object of another type is an error,
// and a blob larger than MaxReadBlob wraps ErrTooLarge.
func (t *TargetRepo) ReadBlob(ctx context.Context, oid string) ([]byte, error) {
	if !isOID(oid) {
		return nil, fmt.Errorf("invalid object id %q", abbrev(oid))
	}
	objs, err := t.catFile(ctx, []string{oid}, MaxReadBlob)
	if err != nil {
		return nil, err
	}
	o := objs[0]
	switch {
	case o.missing:
		return nil, fmt.Errorf("blob %s: %w", oid, ErrNotFound)
	case o.typ != "blob":
		return nil, fmt.Errorf("object %s is a %s, not a blob", oid, o.typ)
	}
	return o.content, nil
}

// resolve returns the full object id of rev ("<ref>^{commit}", …).
func (t *TargetRepo) resolve(ctx context.Context, rev string) (string, error) {
	out, err := t.run(ctx, cmdOpts{}, "rev-parse", "--verify", "--quiet", "--end-of-options", rev)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == 1 {
			return "", fmt.Errorf("%s: %w", rev, ErrNotFound)
		}
		return "", err
	}
	id := trimEOL(out)
	if !isOID(id) {
		return "", fmt.Errorf("git rev-parse %s: unexpected output %q", rev, abbrev(id))
	}
	return id, nil
}

// missing returns the ids of ids that are not present locally, in order
// (git cat-file --batch-check; never fetches).
func (t *TargetRepo) missing(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var in bytes.Buffer
	for _, id := range ids {
		in.WriteString(id)
		in.WriteByte('\n')
	}
	out, err := t.run(ctx, cmdOpts{stdin: &in}, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(ids) {
		return nil, fmt.Errorf("git cat-file --batch-check: %d lines for %d objects", len(lines), len(ids))
	}
	var gone []string
	for i, line := range lines {
		h, err := parseObjectHeader(line, ids[i])
		if err != nil {
			return nil, fmt.Errorf("git cat-file --batch-check: %w", err)
		}
		if h.missing {
			gone = append(gone, ids[i])
		}
	}
	return gone, nil
}

// batchObject is one response of `git cat-file --batch`.
type batchObject struct {
	oid     string
	typ     string
	content []byte
	missing bool
}

// catFile reads objects by name (object ids or revisions such as
// "<rev>^{commit}"; never fetches). A name git cannot resolve, or whose
// object is not present, is missing. An object larger than limit fails the
// whole read with ErrTooLarge before any content is read.
//
// Two commands: `git cat-file --batch-check` resolves the names and sizes
// the objects, then `git cat-file --batch` reads the objects by the ids it
// found, so a ref that moves in between cannot swap in an object that was
// not sized.
func (t *TargetRepo) catFile(ctx context.Context, names []string, limit int64) ([]batchObject, error) {
	var in bytes.Buffer
	for _, n := range names {
		if n == "" || strings.ContainsAny(n, "\n\r\x00") {
			return nil, fmt.Errorf("invalid object name %q", abbrev(n))
		}
		in.WriteString(n)
		in.WriteByte('\n')
	}
	out, err := t.run(ctx, cmdOpts{stdin: &in}, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	heads, err := parseBatchCheck(out, names)
	if err != nil {
		return nil, err
	}
	var ids []string
	in.Reset()
	for _, h := range heads {
		if h.missing {
			continue
		}
		if h.size > limit {
			return nil, fmt.Errorf("%s %s has %d bytes, more than %d: %w", h.typ, h.oid, h.size, limit, ErrTooLarge)
		}
		ids = append(ids, h.oid)
		in.WriteString(h.oid)
		in.WriteByte('\n')
	}
	objs := make([]batchObject, len(names))
	if len(ids) == 0 {
		for i := range objs {
			objs[i].missing = true
		}
		return objs, nil
	}
	out, err = t.run(ctx, cmdOpts{stdin: &in}, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	got, err := parseBatch(out, ids)
	if err != nil {
		return nil, err
	}
	for i, h := range heads {
		if h.missing {
			objs[i].missing = true
			continue
		}
		objs[i], got = got[0], got[1:]
	}
	return objs, nil
}

// parseBatchCheck parses `git cat-file --batch-check` output for names:
// per name "<oid> <type> <size>", or "<name> missing" (or "ambiguous").
func parseBatchCheck(out []byte, names []string) ([]objectHeader, error) {
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(names) {
		return nil, fmt.Errorf("git cat-file --batch-check: %d lines for %d objects", len(lines), len(names))
	}
	heads := make([]objectHeader, len(names))
	for i, line := range lines {
		if rest, ok := strings.CutPrefix(line, names[i]+" "); ok && (rest == "missing" || rest == "ambiguous") {
			heads[i] = objectHeader{missing: true}
			continue
		}
		f := strings.Split(line, " ")
		if len(f) != 3 || !isOID(f[0]) {
			return nil, fmt.Errorf("git cat-file --batch-check: unexpected line %q for %s", abbrev(line), abbrev(names[i]))
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("git cat-file --batch-check: bad object size in %q", abbrev(line))
		}
		heads[i] = objectHeader{oid: f[0], typ: f[1], size: size}
	}
	return heads, nil
}

// parseBatch parses `git cat-file --batch` output for names: per name a
// header "<oid> <type> <size>" followed by the content and a newline, or
// "<name> missing" (or "ambiguous").
func parseBatch(out []byte, names []string) ([]batchObject, error) {
	objs := make([]batchObject, 0, len(names))
	for _, name := range names {
		nl := bytes.IndexByte(out, '\n')
		if nl < 0 {
			return nil, fmt.Errorf("git cat-file --batch: output ends before %s", abbrev(name))
		}
		header := string(out[:nl])
		out = out[nl+1:]
		if rest, ok := strings.CutPrefix(header, name+" "); ok && (rest == "missing" || rest == "ambiguous") {
			objs = append(objs, batchObject{missing: true})
			continue
		}
		f := strings.Split(header, " ")
		if len(f) != 3 || !isOID(f[0]) {
			return nil, fmt.Errorf("git cat-file --batch: unexpected header %q for %s", abbrev(header), abbrev(name))
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size < 0 || size+1 > len(out) || out[size] != '\n' {
			return nil, fmt.Errorf("git cat-file --batch: bad object size in %q", abbrev(header))
		}
		objs = append(objs, batchObject{oid: f[0], typ: f[1], content: slices.Clone(out[:size])})
		out = out[size+1:]
	}
	if len(out) > 0 {
		return nil, fmt.Errorf("git cat-file --batch: %d unexpected bytes after the last object", len(out))
	}
	return objs, nil
}
