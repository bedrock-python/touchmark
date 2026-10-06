package provenance

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// lfsConfig is the Git LFS configuration file. git-lfs reads it only at the
// top of a repository, where it can redirect LFS traffic (lfs.url) for the
// whole repository, so a pack may not ship it there. Deeper copies are inert
// and allowed.
const lfsConfig = ".lfsconfig"

// CheckPacks returns the defects of the packs in cur that `touchmark check`
// reports as errors:
//
//   - a pack name that is not lowercase words of letters and digits joined by
//     single hyphens, or is longer than 64 characters;
//   - a path that fails pathx.Validate (this covers .git and a top-level
//     .gitmodules; .gitmodules deeper down is inert and allowed);
//   - the opt-in file optInName (compared case-insensitively; "" skips the
//     check): each repository writes its own;
//   - a top-level .lfsconfig (compared case-insensitively);
//   - a file smaller than MinEvidenceSize, whose content could never prove
//     that the hub shipped it;
//   - a mode other than 100644 and 100755;
//   - paths that collide on case-insensitive filesystems: two spellings of a
//     file or directory that differ only by case, in one pack or across
//     packs, and a name that is a file in one pack and a directory in
//     another. The same path shipped by two packs is an override, which is
//     allowed.
//
// Problems are sorted by pack, path and message.
func CheckPacks(cur Current, optInName string) []Problem {
	var out []Problem
	for _, pack := range sortedKeys(cur) {
		if err := checkPackName(pack); err != nil {
			out = append(out, Problem{Pack: pack, Message: err.Error()})
		}
		files := cur[pack]
		for _, p := range sortedKeys(files) {
			out = append(out, checkFile(pack, p, files[p], optInName)...)
		}
	}
	out = append(out, caseCollisions(cur)...)
	sortProblems(out)
	return out
}

// checkFile returns the problems of one file shipped by pack at p.
func checkFile(pack, p string, f File, optInName string) []Problem {
	var out []Problem
	add := func(format string, args ...any) {
		out = append(out, Problem{Pack: pack, Path: p, Message: fmt.Sprintf(format, args...)})
	}
	if err := pathx.Validate(p); err != nil {
		add("%v", err)
		return out
	}
	if optInName != "" && pathx.Fold(p) == pathx.Fold(optInName) {
		add("the opt-in file is never shipped: each repository writes its own")
	}
	if pathx.Fold(p) == lfsConfig {
		add("a top-level %s is never shipped: it would configure Git LFS for the whole repository", lfsConfig)
	}
	if f.Size < MinEvidenceSize {
		add("%d bytes, below the %d-byte minimum: content this small cannot prove the hub shipped it", f.Size, MinEvidenceSize)
	}
	if f.Mode != modeFile && f.Mode != modeExec {
		add("unsupported mode %q: want %s or %s", f.Mode, modeFile, modeExec)
	}
	return out
}

// occurrence is a file or directory a pack creates in a target.
type occurrence struct {
	pack string
	path string
	dir  bool
}

func (o occurrence) kind() string {
	if o.dir {
		return "directory"
	}
	return "file"
}

// caseCollisions reports files and directories that would land on the same
// name on a case-insensitive filesystem without being the same entry.
//
// Occurrences are grouped by folded path. Within a group, sorted by path,
// kind and pack, the first one is the reference; every other occurrence that
// is not the same entry as the reference gets one problem.
func caseCollisions(cur Current) []Problem {
	groups := map[string]map[occurrence]bool{}
	add := func(o occurrence) {
		key := pathx.Fold(o.path)
		if groups[key] == nil {
			groups[key] = map[occurrence]bool{}
		}
		groups[key][o] = true
	}
	for pack, files := range cur {
		for p := range files {
			if pathx.Validate(p) != nil {
				continue // reported by checkFile
			}
			add(occurrence{pack: pack, path: p})
			for _, d := range pathx.Parents(p) {
				add(occurrence{pack: pack, path: d, dir: true})
			}
		}
	}
	var out []Problem
	for _, key := range sortedKeys(groups) {
		if len(groups[key]) < 2 {
			continue
		}
		occ := sortedOccurrences(groups[key])
		ref := occ[0]
		for _, o := range occ[1:] {
			if o.path != ref.path || o.dir != ref.dir {
				out = append(out, collision(ref, o))
			}
		}
	}
	return out
}

// sortedOccurrences returns the set sorted by path, files before
// directories, then pack.
func sortedOccurrences(set map[occurrence]bool) []occurrence {
	out := slices.Collect(maps.Keys(set))
	slices.SortFunc(out, func(a, b occurrence) int {
		return cmp.Or(
			cmp.Compare(a.path, b.path),
			cmp.Compare(boolInt(a.dir), boolInt(b.dir)),
			cmp.Compare(a.pack, b.pack),
		)
	})
	return out
}

// collision describes how o clashes with the reference occurrence ref.
func collision(ref, o occurrence) Problem {
	where := fmt.Sprintf("in pack %q", ref.pack)
	if ref.pack == o.pack {
		where = "in the same pack"
	}
	var msg string
	if o.path == ref.path {
		msg = fmt.Sprintf("%s collides with a %s of the same name %s", o.kind(), ref.kind(), where)
	} else {
		msg = fmt.Sprintf("%s differs only by case from %s %q %s: they collide on case-insensitive filesystems",
			o.kind(), ref.kind(), ref.path, where)
	}
	return Problem{Pack: o.pack, Path: o.path, Message: msg}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
