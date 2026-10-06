package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// testNameRe finds the tests and fuzz targets a document names in code
// spans, and testFuncRe their declarations.
var (
	testNameRe = regexp.MustCompile("`((?:Test|Fuzz)[A-Z][A-Za-z0-9_]*)`")
	testFuncRe = regexp.MustCompile(`(?m)^func ((?:Test|Fuzz)[A-Z][A-Za-z0-9_]*)\(`)
)

// TestThreatModelCitesTests keeps docs/project/threat-model.md accurate to
// the code: every test it names exists in some package of the module (e2e
// tests with their build tag included), every threat T1 to T9 has its row,
// and every row names a test or a residual risk.
func TestThreatModelCitesTests(t *testing.T) {
	root := filepath.Join(pkgDir, "..", "..")
	doc, err := os.ReadFile(filepath.Join(root, "docs", "project", "threat-model.md"))
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	err = filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range testFuncRe.FindAllStringSubmatch(string(data), -1) {
			defined[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !defined["TestThreatModelCitesTests"] {
		t.Fatal("the walk found no tests: update the test")
	}
	cited := testNameRe.FindAllStringSubmatch(string(doc), -1)
	if len(cited) < 100 {
		t.Errorf("the threat model names only %d tests", len(cited))
	}
	var missing []string
	for _, m := range cited {
		if !defined[m[1]] && !slices.Contains(missing, m[1]) {
			missing = append(missing, m[1])
		}
	}
	if len(missing) > 0 {
		t.Errorf("docs/project/threat-model.md names tests that do not exist: %s", strings.Join(missing, ", "))
	}

	// Every threat has its row, and every row of every table a test or a
	// residual risk.
	for _, id := range []string{"T1", "T2", "T3", "T4", "T5", "T6", "T7", "T8", "T9"} {
		if !strings.Contains(string(doc), "\n| "+id+". ") {
			t.Errorf("docs/project/threat-model.md has no row for %s", id)
		}
	}
	for i, line := range strings.Split(string(doc), "\n") {
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if !strings.HasPrefix(line, "| ") || len(cells) != 4 || strings.HasPrefix(line, "|---") || cells[2] == "Tests" {
			continue
		}
		tests, residual := cells[2], strings.TrimSpace(cells[3])
		if !testNameRe.MatchString(tests) && !strings.HasPrefix(tests, "see ") && (residual == "" || residual == "—") {
			t.Errorf("docs/project/threat-model.md:%d: a row with neither a test nor a residual risk: %s", i+1, line)
		}
	}
}
