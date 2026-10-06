package gitx

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
)

// attributesFile is the name of in-tree attribute files.
const attributesFile = ".gitattributes"

// Attrs returns, for each path, the values of attrs that the
// .gitattributes files of tree give it (git check-attr --source=<tree>
// -z); values are "set", "unset", "unspecified" or the value string. The
// .gitattributes blobs must be present (FetchBlobs).
//
// tree may be a tree or a commit. Git reads a missing .gitattributes blob
// as an empty file, so Attrs first checks that every .gitattributes entry
// (regular file or symlink) in the root and in the directories above the
// paths is present, and fails with an error wrapping ErrNotFound naming the
// first missing one. Only the tree's files count: the repository has no
// info/attributes, and neither the system nor the user attribute file is
// read (GIT_ATTR_NOSYSTEM, an empty HOME). Needs git 2.40 or newer.
//
// Paths are repository paths; attribute names follow git's rules (ASCII
// letters, digits, '-', '.', '_', not starting with '-'). At least one
// attribute is required; no paths give an empty map.
func (t *TargetRepo) Attrs(ctx context.Context, tree string, paths []string, attrs ...string) (map[string]map[string]string, error) {
	if len(attrs) == 0 {
		return nil, fmt.Errorf("git check-attr: no attribute named")
	}
	for _, a := range attrs {
		if !isAttrName(a) {
			return nil, fmt.Errorf("git check-attr: invalid attribute name %q", abbrev(a))
		}
	}
	for _, p := range paths {
		if err := checkTreePath(p); err != nil {
			return nil, fmt.Errorf("git check-attr: %w", err)
		}
	}
	if len(paths) == 0 {
		return map[string]map[string]string{}, nil
	}
	treeID, err := t.attributesSource(ctx, tree, paths)
	if err != nil {
		return nil, err
	}
	return t.checkAttr(ctx, treeID, paths, attrs)
}

// checkAttr runs git check-attr for valid paths and attrs on treeID, whose
// attribute files are present.
func (t *TargetRepo) checkAttr(ctx context.Context, treeID string, paths, attrs []string) (map[string]map[string]string, error) {
	got := map[string]map[string]string{}
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString(p)
		in.WriteByte(0)
	}
	args := append([]string{"check-attr", "--source=" + treeID, "-z", "--stdin"}, attrs...)
	out, err := t.run(ctx, cmdOpts{stdin: &in}, append(args, "--")...)
	if err != nil {
		return nil, err
	}
	toks := strings.Split(string(out), "\x00")
	if len(toks)%3 != 1 || toks[len(toks)-1] != "" {
		return nil, fmt.Errorf("git check-attr: unexpected output of %d fields", len(toks))
	}
	for i := 0; i+2 < len(toks); i += 3 {
		p, attr, value := toks[i], toks[i+1], toks[i+2]
		if !slices.Contains(attrs, attr) {
			return nil, fmt.Errorf("git check-attr: unexpected attribute %q", abbrev(attr))
		}
		if got[p] == nil {
			got[p] = map[string]string{}
		}
		got[p][attr] = value
	}
	for _, p := range paths {
		if len(got[p]) != len(attrs) {
			return nil, fmt.Errorf("git check-attr: no answer for %q", abbrev(p))
		}
	}
	return got, nil
}

// Renormalized returns the id the blob of content has once in the target
// under the attributes of tree: what `git add` would store for the file of
// a checkout, so a result that differs from the blob's own id means the
// target's attributes rewrite it, and the path is unsafe (renormalize).
//
// It is git --attr-source=<tree> hash-object --path=<path> --stdin:
// --attr-source (git 2.41, the same as GIT_ATTR_SOURCE) makes hash-object
// read attributes from tree, so eol, text, ident and working-tree-encoding
// apply as in `git add`, with core.autocrlf=false and core.safecrlf=false.
// Filter drivers are never configured, so filter= does not run anything.
// The attribute blobs must be present, as for Attrs. Nothing is written.
//
// One case differs from hash-object: under text=auto git keeps the CRLF of
// a blob that is already in the index as it is (convert.c,
// has_crlf_in_index), and once delivered the blob is in the index of every
// checkout, whose status stays clean. So when hash-object changes the blob
// and the path's text attribute is auto, with neither ident nor
// working-tree-encoding set, the blob's own id is returned.
func (t *TargetRepo) Renormalized(ctx context.Context, tree, path string, content []byte) (string, error) {
	if err := checkTreePath(path); err != nil {
		return "", fmt.Errorf("git hash-object: %w", err)
	}
	treeID, err := t.attributesSource(ctx, tree, []string{path})
	if err != nil {
		return "", err
	}
	out, err := t.run(ctx, cmdOpts{stdin: bytes.NewReader(content)},
		"--attr-source="+treeID, "-c", "core.safecrlf=false",
		"hash-object", "--path="+path, "--stdin")
	if err != nil {
		return "", err
	}
	id := trimEOL(out)
	if !isOID(id) {
		return "", fmt.Errorf("git hash-object: unexpected output %q", abbrev(id))
	}
	own := RawOID(content)
	if id == own {
		return id, nil
	}
	attrs, err := t.checkAttr(ctx, treeID, []string{path}, []string{"text", "ident", "working-tree-encoding"})
	if err != nil {
		return "", err
	}
	if a := attrs[path]; a["text"] == "auto" && a["ident"] != "set" && a["working-tree-encoding"] == "unspecified" {
		return own, nil
	}
	return id, nil
}

// attributesSource resolves tree to a tree id and checks that the
// .gitattributes blobs git reads for paths are present.
func (t *TargetRepo) attributesSource(ctx context.Context, tree string, paths []string) (string, error) {
	if err := checkObjectName(tree); err != nil {
		return "", err
	}
	treeID, err := t.resolve(ctx, tree+"^{tree}")
	if err != nil {
		return "", err
	}
	files, err := t.attributeFiles(ctx, treeID)
	if err != nil {
		return "", err
	}
	dirs := map[string]bool{"": true}
	for _, p := range paths {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var ids, names []string
	for _, e := range files {
		dir := path.Dir(e.Path)
		if dir == "." {
			dir = ""
		}
		if dirs[dir] {
			ids = append(ids, e.OID)
			names = append(names, e.Path)
		}
	}
	missing, err := t.missing(ctx, ids)
	if err != nil {
		return "", err
	}
	if len(missing) > 0 {
		name := names[slices.Index(ids, missing[0])]
		return "", fmt.Errorf("%s (blob %s) of tree %s is not present, fetch it first: %w", name, missing[0], treeID, ErrNotFound)
	}
	return treeID, nil
}

// attributeFiles returns the .gitattributes entries (regular files and
// symlinks, in every directory) of the tree treeID. The tree is listed once
// per repository and the answer kept: a tree id names its content for good,
// and the renormalize check asks for every path of D, up
// to three rounds, on trees of any size.
func (t *TargetRepo) attributeFiles(ctx context.Context, treeID string) ([]TreeEntry, error) {
	x := t.iso
	x.attrMu.Lock()
	files, ok := x.attrFiles[treeID]
	x.attrMu.Unlock()
	if ok {
		return files, nil
	}
	entries, err := t.Tree(ctx, treeID)
	if err != nil {
		return nil, err
	}
	files = []TreeEntry{}
	for _, e := range entries {
		if path.Base(e.Path) == attributesFile && e.Type == "blob" {
			files = append(files, e)
		}
	}
	x.attrMu.Lock()
	if x.attrFiles == nil {
		x.attrFiles = map[string][]TreeEntry{}
	}
	x.attrFiles[treeID] = files
	x.attrMu.Unlock()
	return files, nil
}

// isAttrName reports whether a is an attribute name git accepts
// (attr.c attr_name_valid): letters, digits, '-', '.', '_', not starting
// with '-'.
func isAttrName(a string) bool {
	if a == "" || a[0] == '-' {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c != '-' && c != '.' && c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
