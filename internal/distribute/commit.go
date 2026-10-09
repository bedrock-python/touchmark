package distribute

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/snapshot"
	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// maxRounds bounds the unsafe checks of settle: taking a path out of D
// changes the tree, so the checks run again, at most this many times.
const maxRounds = 3

// maxHubBlob bounds a hub blob read into memory for the renormalize check,
// as gitx.MaxReadBlob bounds a target's blobs: a larger one that the check
// needs fails the target (failed:git).
const maxHubBlob = gitx.MaxReadBlob

// errNoFixpoint: the unsafe checks still took paths out of D after
// maxRounds rounds.
var errNoFixpoint = errors.New("the checks for unsafe paths did not settle")

// Attributes that make a path unsafe to deliver when a .gitattributes gives
// them a value, in B or in the new tree, and those that may make git store a
// file as another blob than the hub's.
const (
	attrFilter   = "filter"
	attrEncoding = "working-tree-encoding"
	attrText     = "text"
	attrEOL      = "eol"
	attrIdent    = "ident"
)

// attributesFile is the name of in-tree attribute files.
const attributesFile = ".gitattributes"

// settle runs step 8: the per-path plan and D, the commit of D on B and the
// unsafe checks on B and on the new tree. A path they find unsafe leaves D
// (its state becomes unsafe, with a target warning), and D and the commit
// are made again, until the checks find nothing new; after maxRounds rounds
// that still found something the target fails. w gets Plan, D, Key and Built
// (zero when D is empty).
func (r *run) settle(ctx context.Context, w *Work, optIn *config.OptIn, sel config.Selection) error {
	c := &checker{r: r, w: w}
	unsafe := map[string]string{}
	for round := 1; ; round++ {
		w.Plan = decide.Decide(r.decideInput(optIn, sel, w.Tree, unsafe))
		w.D = decide.Pairs(w.Plan)
		w.Key, w.Built = "", gitx.Built{}
		if len(w.D) == 0 {
			return nil
		}
		w.Key = decide.Key(decide.StreamSync, w.D)
		found := treeConflicts(w.Tree, w.D)
		if len(found) == 0 {
			built, err := r.build(ctx, w)
			if err != nil {
				return err
			}
			w.Built = built
			if found, err = c.check(ctx); err != nil {
				return err
			}
		}
		if len(found) == 0 {
			return nil
		}
		for _, p := range slices.Sorted(maps.Keys(found)) {
			unsafe[p] = found[p]
			w.t.res.Warnings = append(w.t.res.Warnings, fmt.Sprintf("unsafe: %s: %s; it is left as it is", p, found[p]))
		}
		if round == maxRounds {
			return fmt.Errorf("%w in %d rounds", errNoFixpoint, maxRounds)
		}
	}
}

// written returns the paths of pairs that write content (not deletions),
// sorted.
func written(pairs []decide.Pair) []string {
	var out []string
	for _, p := range pairs {
		if p.Mode != decide.ModeDelete {
			out = append(out, p.Path)
		}
	}
	slices.Sort(out)
	return out
}

// treeConflicts returns the written paths of d that the new tree (tree with
// d applied) could not hold safely, with why: a parent of the path is a
// file of the new tree, the path is a directory of it, or a name the path
// brings (the path, or a directory above it that B does not have) differs
// only by case from another name of the new tree. Observe finds these
// against B; here they are found among the paths D adds.
func treeConflicts(tree *snapshot.Tree, d []decide.Pair) map[string]string {
	old := names(tree.Entries)
	entries := map[string]bool{}
	for p := range tree.Entries {
		entries[p] = true
	}
	for _, p := range d {
		if p.Mode == decide.ModeDelete {
			delete(entries, p.Path)
		} else {
			entries[p.Path] = true
		}
	}
	now := names(entries)
	folds := map[string][]string{}
	for _, n := range slices.Sorted(maps.Keys(now)) {
		folds[pathx.Fold(n)] = append(folds[pathx.Fold(n)], n)
	}
	found := map[string]string{}
	for _, p := range written(d) {
		if why := conflict(p, entries, now, old, folds); why != "" {
			found[p] = why
		}
	}
	return found
}

// conflict says why the new tree cannot hold the written path p, "" when
// it can (see treeConflicts): the topmost parent that is a file, then p
// being a directory, then the topmost name p brings that clashes by case
// with another name (the smallest such).
func conflict(p string, entries map[string]bool, now, old map[string]int, folds map[string][]string) string {
	for _, a := range pathx.Parents(p) {
		if entries[a] {
			return fmt.Sprintf("its parent %q is a file of the new tree", a)
		}
	}
	if now[p] == nameDir {
		return "it is a directory of the new tree"
	}
	for _, q := range append(pathx.Parents(p), p) {
		if old[q] != 0 {
			continue // B has it: any clash is B's own
		}
		for _, other := range folds[pathx.Fold(q)] {
			if other != q {
				return fmt.Sprintf("%q differs only by case from %q of the new tree", q, other)
			}
		}
	}
	return ""
}

// Kinds of the names of a tree.
const (
	nameFile = 1 // an entry (a file, symlink or submodule)
	nameDir  = 2 // a directory some entry lies in
)

// names returns every name of a tree whose entries are the keys of set:
// the entries, and the directories they lie in. A name that is both counts
// as a directory.
func names[V any](set map[string]V) map[string]int {
	out := make(map[string]int, len(set))
	for p := range set {
		if out[p] == 0 {
			out[p] = nameFile
		}
		for _, a := range pathx.Parents(p) {
			out[a] = nameDir
		}
	}
	return out
}

// checker runs the attribute checks of one target's rounds. The .gitattributes
// blobs of B are fetched once, and the attributes of B read once, for the
// written paths of the first round that builds a commit: later rounds only
// take paths out of D.
type checker struct {
	r       *run
	w       *Work
	fetched bool
	base    map[string]map[string]string
}

// check returns the written paths of D that the attributes of B or of the
// commit's tree make unsafe, with why: filter= (LFS included) or
// working-tree-encoding in either tree, or a blob git would store as another
// one under the new tree's attributes (renormalize).
func (c *checker) check(ctx context.Context) (map[string]string, error) {
	w := c.w
	paths := written(w.D)
	if len(paths) == 0 || !hasAttributes(w.Tree, w.D) {
		return nil, nil
	}
	if !c.fetched {
		err := c.r.gitRetry(ctx, w.t.prov, func() error { return gitFailure(w.Repo.FetchBlobs(ctx, attributeBlobs(w.Tree))) })
		if err != nil {
			return nil, fmt.Errorf("fetch the .gitattributes files of %s: %w", short(w.B), err)
		}
		c.fetched = true
	}
	if c.base == nil {
		base, err := w.Repo.Attrs(ctx, w.B, paths, attrFilter, attrEncoding)
		if err != nil {
			return nil, fmt.Errorf("read the attributes of %s: %w", short(w.B), err)
		}
		c.base = base
	}
	now, err := w.Repo.Attrs(ctx, w.Built.Tree, paths, attrFilter, attrEncoding, attrText, attrEOL, attrIdent)
	if err != nil {
		return nil, fmt.Errorf("read the attributes of the new tree: %w", err)
	}
	found := map[string]string{}
	blobs := map[string]string{}
	for _, p := range w.D {
		blobs[p.Path] = p.To
	}
	for _, p := range paths {
		if why := unsafeAttrs(c.base[p], now[p]); why != "" {
			found[p] = why
			continue
		}
		if !converts(now[p]) {
			continue
		}
		same, err := c.keepsBlob(ctx, p, blobs[p])
		if err != nil {
			return nil, err
		}
		if !same {
			found[p] = "the target's .gitattributes would make git store another blob than the hub's (renormalize)"
		}
	}
	return found, nil
}

// keepsBlob reports whether the hub blob oid, written at path, is stored as
// itself under the attributes of the new tree.
func (c *checker) keepsBlob(ctx context.Context, path, oid string) (bool, error) {
	content, err := c.r.hubBlob(oid)
	if err != nil {
		return false, err
	}
	id, err := c.w.Repo.Renormalized(ctx, c.w.Built.Tree, path, content)
	if err != nil {
		return false, fmt.Errorf("renormalize %s: %w", path, err)
	}
	return id == oid, nil
}

// hubBlob reads a hub blob whole (Write.HubBlobs), at most maxHubBlob
// bytes.
func (r *run) hubBlob(oid string) ([]byte, error) {
	rc, err := r.d.Write.HubBlobs(oid)
	if err != nil {
		return nil, fmt.Errorf("hub blob %s: %w", oid, err)
	}
	content, err := io.ReadAll(io.LimitReader(rc, maxHubBlob+1))
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return nil, fmt.Errorf("hub blob %s: %w", oid, err)
	case len(content) > maxHubBlob:
		return nil, fmt.Errorf("hub blob %s has more than %d bytes: %w", oid, maxHubBlob, gitx.ErrTooLarge)
	}
	return content, nil
}

// unsafeAttrs says why the attributes of a path in B (base) or in the new
// tree (now) make it unsafe, "" when they do not.
func unsafeAttrs(base, now map[string]string) string {
	for _, a := range []string{attrFilter, attrEncoding} {
		for _, side := range []struct {
			name  string
			attrs map[string]string
		}{{"default branch", base}, {"new tree", now}} {
			if v := side.attrs[a]; valued(v) {
				return fmt.Sprintf("the %s's .gitattributes gives it %s=%s", side.name, a, v)
			}
		}
	}
	return ""
}

// valued reports whether an attribute value from git check-attr assigns
// something: set or a value, not unset or unspecified.
func valued(v string) bool { return v != "" && v != "unspecified" && v != "unset" }

// converts reports whether the attributes of a path may make git store a
// file as another blob: text or eol conversion, or ident (the new tree's
// filter and working-tree-encoding make the path unsafe before this). With
// none of them and core.autocrlf off, git stores the bytes as they are.
func converts(attrs map[string]string) bool {
	return valued(attrs[attrText]) || valued(attrs[attrEOL]) || attrs[attrIdent] == "set"
}

// hasAttributes reports whether B (tree) or the paths D writes hold a
// .gitattributes file. Without one git gives every path unspecified
// attributes (only the trees' files count: GIT_ATTR_NOSYSTEM, no user
// file, no info/attributes), so the attribute checks find nothing.
func hasAttributes(tree *snapshot.Tree, d []decide.Pair) bool {
	if len(attributeBlobs(tree)) > 0 {
		return true
	}
	return slices.ContainsFunc(d, func(p decide.Pair) bool {
		return p.Mode != decide.ModeDelete && path.Base(p.Path) == attributesFile
	})
}

// attributeBlobs returns the blob ids of every .gitattributes entry of tree
// (regular files and symlinks, as git reads them), sorted.
func attributeBlobs(tree *snapshot.Tree) []string {
	var out []string
	for p, e := range tree.Entries {
		if path.Base(p) == attributesFile && (e.Mode == "100644" || e.Mode == "100755" || e.Mode == "120000") && !slices.Contains(out, e.OID) {
			out = append(out, e.OID)
		}
	}
	slices.Sort(out)
	return out
}

// build writes the commit of D on B: hub blobs, the writer as author and
// committer, dated max(hub commit, B), the commit message with the trailers,
// signed with the provider's key when it has one.
func (r *run) build(ctx context.Context, w *Work) (gitx.Built, error) {
	p := w.t.prov
	msg, err := prbody.CommitMessage(r.hub.Commit.Message, decide.FormatTrailers(decide.Trailers{
		HubID:       r.hub.ID,
		Fingerprint: r.fps[0],
		Stream:      decide.StreamSync,
		Content:     w.Key,
		HubCommit:   r.d.HubCommit,
	}))
	if err != nil {
		return gitx.Built{}, err
	}
	when, err := r.commitTime(ctx, w)
	if err != nil {
		return gitx.Built{}, err
	}
	spec := gitx.CommitSpec{Parent: w.B, Blobs: map[string]gitx.Blob{}, When: when, Message: msg}
	spec.Author = r.commitPerson(p)
	spec.Committer = spec.Author
	for _, pair := range w.D {
		if pair.Mode == decide.ModeDelete {
			spec.Changes = append(spec.Changes, gitx.Change{Path: pair.Path})
			continue
		}
		spec.Changes = append(spec.Changes, gitx.Change{Path: pair.Path, Mode: pair.Mode, OID: pair.To})
		oid := pair.To
		spec.Blobs[oid] = gitx.Blob{OID: oid, Open: func() (io.ReadCloser, error) { return r.d.Write.HubBlobs(oid) }}
	}
	if s := p.signer; s != nil {
		spec.Sign = func(payload []byte) (string, error) { return s.Sign(sshsig.Namespace, payload) }
	}
	return w.Repo.BuildCommit(ctx, spec)
}

// commitTime is the date of the commit: the later of the hub commit's and
// B's committer dates, so the same inputs give the same commit; the Unix
// epoch when neither is known.
func (r *run) commitTime(ctx context.Context, w *Work) (time.Time, error) {
	b, err := w.Repo.Commit(ctx, w.B)
	if err != nil {
		return time.Time{}, fmt.Errorf("read %s: %w", short(w.B), err)
	}
	when := r.d.Write.HubCommitTime
	if b.Time.After(when) {
		when = b.Time
	}
	if when.IsZero() {
		when = time.Unix(0, 0)
	}
	return when.UTC(), nil
}

// commitPerson is the author and committer of the provider's commits: the
// account the write credential acts as (in a plan, the writer hub.yml names
// as Lookup found it), with its email, or the platform's no-reply address
// when the platform gives none.
func (r *run) commitPerson(p *provider) gitx.Person {
	a := p.self
	if a.Login == "" {
		a = p.writerAcct
	}
	name := a.Login
	if name == "" {
		name = p.cfg.Writer
	}
	if name == "" {
		name = "touchmark"
	}
	email := a.Email
	if email == "" {
		email = noReplyEmail(p.cfg.Type, p.cfg.Host, a.ID, name)
	}
	return gitx.Person{Name: name, Email: email}
}

// noReplyEmail is the address a platform gives an account that keeps its
// email private: GitHub "<id>+<login>@users.noreply.<host>", GitLab
// "<id>-<login>@users.noreply.<host>", Gitea and Forgejo
// "<login>@noreply.<host>". host loses its port.
//
// Bitbucket Cloud has no such address: it links a commit to the account one
// of whose confirmed addresses is the author's, and the driver gives the
// account's primary confirmed address when GET /2.0/user/emails shows one.
// Without it, the author is "<uuid>@touchmark.invalid", the account's UUID
// without braces under a domain that can never exist (RFC 6761, .invalid):
// the commit then names the bot by its UUID, links to no account, and
// can never be taken for someone else's. Azure DevOps has none either: its
// driver gives the user's sign-in address when connectionData shows one,
// else the author is "<identity id>@touchmark.invalid".
func noReplyEmail(typ, host, id, login string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	switch typ {
	case "bitbucket", "azure-devops":
		return strings.Trim(cmp.Or(id, login), "{}") + "@touchmark.invalid"
	case "gitlab":
		if id != "" {
			return id + "-" + login + "@users.noreply." + host
		}
		return login + "@users.noreply." + host
	case "gitea", "forgejo":
		return login + "@noreply." + host
	}
	if id != "" {
		return id + "+" + login + "@users.noreply." + host
	}
	return login + "@users.noreply." + host
}
