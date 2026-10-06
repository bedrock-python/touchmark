package provenance

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Pack names follow the same rules as in hub.yml: lowercase letters and
// digits in words joined by single hyphens, at most maxPackNameLen bytes.
const maxPackNameLen = 64

var packNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Tree entry modes of the files a pack ships.
const (
	modeFile = "100644"
	modeExec = "100755"
)

// checkPackName validates a pack directory name. Error messages quote name,
// so they are safe to print.
func checkPackName(name string) error {
	switch {
	case name == "":
		return errors.New("empty pack name")
	case len(name) > maxPackNameLen:
		return fmt.Errorf("pack name %.40q… is longer than %d characters", name, maxPackNameLen)
	case !packNameRe.MatchString(name):
		return fmt.Errorf("pack name %q must be lowercase letters and digits in words joined by single hyphens", name)
	}
	return nil
}

// splitHubPath splits a hub path "packs/<pack>/<path>" into the pack name
// and the repository path inside targets. ok is false for anything else:
// "packs" itself and files directly under packs/.
func splitHubPath(hubPath string) (pack, path string, ok bool) {
	rest, ok := strings.CutPrefix(hubPath, PacksDir+"/")
	if !ok {
		return "", "", false
	}
	pack, path, ok = strings.Cut(rest, "/")
	if !ok || pack == "" || path == "" {
		return "", "", false
	}
	return pack, path, true
}

// hubPath joins a pack name and a repository path into the hub path
// "packs/<pack>/<path>"; an empty path gives the pack directory.
func hubPath(pack, path string) string {
	if path == "" {
		return PacksDir + "/" + pack
	}
	return PacksDir + "/" + pack + "/" + path
}

// display returns s as is when it prints cleanly, and Go-quoted when it holds
// invalid UTF-8 or characters that are not printable (control characters,
// unusual spaces).
func display(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
		return s
	}
	return strconv.Quote(s)
}

// isOID reports whether s is a full lowercase hex object id (sha1 or sha256).
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sortProblems orders problems by pack, path and message, so output does not
// depend on map iteration or walk order.
func sortProblems(ps []Problem) {
	slices.SortFunc(ps, func(a, b Problem) int {
		return cmp.Or(
			cmp.Compare(a.Pack, b.Pack),
			cmp.Compare(a.Path, b.Path),
			cmp.Compare(a.Message, b.Message),
		)
	})
}

// sortedKeys returns the keys of m in ascending order.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
