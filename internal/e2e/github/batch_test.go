package githube2e

import (
	"bytes"
	"errors"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
)

// TestBatchReader reads one path in many repositories through the
// driver's GraphQL batch (platform.BatchReader) and one by one through
// ReadFile, against the fake: both must agree for a regular file, an
// executable, a binary file, a symlink, a submodule, a directory, a missing
// file, a file over the limit, an empty repository and a repository that
// does not exist. GitHub answers a missing path of Commit.file with null
// and a NOT_FOUND error on the field (TestShapes).
func TestBatchReader(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOptions{flavor: ghfake.DotCom})
	r, _ := w.drivers()
	batch, ok := r.(platform.BatchReader)
	if !ok {
		t.Fatal("the reader is no BatchReader")
	}
	const path = ".engineering-assets.yml"
	binary := []byte("version: 1\n\x00\x01\x02")
	cases := []struct {
		name  string
		files []ghfake.File
		want  error // nil: the file; else the sentinel
	}{
		{"regular", []ghfake.File{{Path: path, Content: []byte("version: 1\n")}}, nil},
		{"executable", []ghfake.File{{Path: path, Mode: ghfake.ModeExecutable, Content: []byte("version: 1\n")}}, nil},
		{"binary", []ghfake.File{{Path: path, Content: binary}}, nil},
		{"symlink", []ghfake.File{{Path: "real.yml", Content: []byte("version: 1\n")}, {Path: path, Mode: ghfake.ModeSymlink, Content: []byte("real.yml")}}, platform.ErrNotRegular},
		{"submodule", []ghfake.File{{Path: "README.md", Content: []byte("x\n")}, {Path: path, Mode: ghfake.ModeGitlink, Content: []byte("0123456789abcdef0123456789abcdef01234567")}}, platform.ErrNotRegular},
		{"directory", []ghfake.File{{Path: path + "/inner.yml", Content: []byte("x\n")}}, platform.ErrNotRegular},
		{"missing", []ghfake.File{{Path: "README.md", Content: []byte("x\n")}}, platform.ErrNotFound},
		{"large", []ghfake.File{{Path: path, Content: bytes.Repeat([]byte("# padding\n"), 200)}}, platform.ErrTooLarge},
		{"empty", nil, platform.ErrNotFound},
	}
	var repos []platform.Repo
	for _, c := range cases {
		created := try(w.srv.CreateRepo(ghfake.RepoSpec{Owner: org, Name: c.name, Files: c.files})).of(t)
		repos = append(repos, w.repo(created, len(c.files) == 0, nil))
	}
	repos = append(repos, platform.Repo{Host: w.host, ID: "999999", Path: org + "/gone", DefaultBranch: "main"})
	cases = append(cases, struct {
		name  string
		files []ghfake.File
		want  error
	}{"gone", nil, platform.ErrNotFound})

	files, err := batch.ReadFiles(t.Context(), repos, path, 1024)
	var fileErrs platform.FileErrors
	if err != nil && !errors.As(err, &fileErrs) {
		t.Fatalf("ReadFiles: %v", err)
	}
	if len(files) != len(repos) {
		t.Fatalf("ReadFiles returned %d files for %d repositories", len(files), len(repos))
	}
	for i, c := range cases {
		var got error
		if fileErrs != nil {
			got = fileErrs[i]
		}
		one, oneErr := r.ReadFile(t.Context(), repos[i], "", path, 1024)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: ReadFiles: %v", c.name, got)
		case c.want != nil && !errors.Is(got, c.want):
			t.Errorf("%s: ReadFiles: %v, want %v", c.name, got, c.want)
		case c.want == nil && (!bytes.Equal(files[i].Content, one.Content) || files[i].Mode != one.Mode || files[i].OID != one.OID):
			t.Errorf("%s: ReadFiles %s %s %q, ReadFile %s %s %q", c.name, files[i].Mode, files[i].OID, files[i].Content, one.Mode, one.OID, one.Content)
		case c.want == nil && oneErr != nil:
			t.Errorf("%s: ReadFile: %v", c.name, oneErr)
		case c.want != nil && !errors.Is(oneErr, c.want) && c.name != "gone":
			t.Errorf("%s: ReadFile: %v, want %v, as ReadFiles", c.name, oneErr, c.want)
		}
	}
	w.violations()
}
