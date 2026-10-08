package distribute

import (
	"context"
	"errors"
	"sync"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Opt-in files: read one by one (Reader.ReadFile), or in batches where the
// provider's reader is a platform.BatchReader (GraphQL: GitHub's aliases,
// GitLab's projects(ids:)), so
// that a plan over thousands of targets reads their opt-in files in a few
// requests.

// optInChunk is how many targets one BatchReader call reads the opt-in
// files of: the batch of GitHub's and GitLab's GraphQL queries.
const optInChunk = 50

// optInBatch reads the opt-in files of one provider's targets in chunks, in
// the order phase C inspects them. A chunk is read when the first of its
// targets needs its file, so the deadline and the provider's circuit apply
// to it as to a single read.
type optInBatch struct {
	p      *provider
	reader platform.BatchReader
}

// optInEntry is one target's place in a chunk; the chunk's once reads the
// whole chunk.
type optInEntry struct {
	chunk *optInGroup
	i     int
}

// optInGroup is one chunk of targets and, once read, their files and
// errors.
type optInGroup struct {
	batch   *optInBatch
	targets []*target
	once    sync.Once
	files   []platform.File
	errs    platform.FileErrors
	// failed is set when the whole call failed: every target of the chunk
	// reads its file on its own.
	failed bool
}

// prepareBatches assigns each target of list whose provider reads opt-in
// files in batches (platform.BatchReader) its chunk: in list order per
// provider, leaving out the targets whose outcome is known without their
// file (hidden in a public hub, or skipped by the resolve's flags), and all
// of them when the plan processes no target.
func (r *run) prepareBatches(list []*target) {
	if r.hubOnly() {
		return
	}
	groups := map[*provider]*optInGroup{}
	for _, t := range list {
		br, ok := t.prov.reader.(platform.BatchReader)
		if !ok || t.hidden || skipReason(t.repo) != "" {
			continue
		}
		if r.batches == nil {
			r.batches = map[*provider]*optInBatch{}
		}
		b := r.batches[t.prov]
		if b == nil {
			b = &optInBatch{p: t.prov, reader: br}
			r.batches[t.prov] = b
		}
		g := groups[t.prov]
		if g == nil || len(g.targets) == optInChunk {
			g = &optInGroup{batch: b}
			groups[t.prov] = g
		}
		t.optIn = &optInEntry{chunk: g, i: len(g.targets)}
		g.targets = append(g.targets, t)
	}
}

// readOptIn reads t's opt-in file at ref ("" for the default branch head):
// from its chunk the first time phase C asks for the default branch head,
// else through Reader.ReadFile with the retries of a read. A file the batch
// could not settle (the whole call failed, or a failure of its own other
// than a missing, irregular or oversized file) is read on its own.
func (r *run) readOptIn(ctx context.Context, t *target, ref string) (platform.File, error) {
	if e := t.optIn; e != nil && ref == "" {
		t.optIn = nil
		if f, settled, err := e.chunk.read(ctx, r, e.i); settled {
			return f, err
		}
	}
	var file platform.File
	err := r.retry(ctx, t.prov, func() error {
		var err error
		file, err = t.prov.reader.ReadFile(ctx, t.repo, ref, r.optIn, maxOptIn)
		return err
	})
	return file, err
}

// read returns the file of the chunk's i-th target, or the error that
// settles it, reading the chunk first when nobody did yet; settled is false
// when the batch did not settle it.
func (g *optInGroup) read(ctx context.Context, r *run, i int) (file platform.File, settled bool, err error) {
	g.once.Do(func() { g.load(ctx, r) })
	if g.failed {
		return platform.File{}, false, nil
	}
	if i < len(g.errs) {
		err = g.errs[i]
	}
	switch {
	case err == nil && i < len(g.files):
		return g.files[i], true, nil
	case isNotFound(err), errors.Is(err, platform.ErrNotRegular), errors.Is(err, platform.ErrTooLarge):
		return platform.File{}, true, err
	}
	return platform.File{}, false, nil
}

// load reads the chunk through the provider's BatchReader, with the retries
// of a read: a transient failure or a rate limit of the whole call is tried
// again, and a call that fails for good leaves every file to be read on its
// own (which the provider's circuit then answers).
func (g *optInGroup) load(ctx context.Context, r *run) {
	repos := make([]platform.Repo, len(g.targets))
	for i, t := range g.targets {
		repos[i] = t.repo
	}
	var files []platform.File
	var errs platform.FileErrors
	err := r.retry(ctx, g.batch.p, func() error {
		var err error
		files, err = g.batch.reader.ReadFiles(ctx, repos, r.optIn, maxOptIn)
		errs = nil
		if errors.As(err, &errs) {
			return nil
		}
		return err
	})
	if err != nil || len(files) != len(repos) || (errs != nil && len(errs) != len(repos)) {
		g.failed = true
		return
	}
	g.files, g.errs = files, errs
}
