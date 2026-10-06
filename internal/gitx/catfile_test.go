package gitx

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

const missingOID = "0123456789abcdef0123456789abcdef01234567"

// blobFixtures writes test blobs, including binary content with NUL bytes
// and a 1 MiB blob, and returns them by id.
func blobFixtures(r *testRepo) map[string][]byte {
	r.t.Helper()
	big := make([]byte, 1<<20)
	rng := rand.New(rand.NewPCG(7, 7))
	for i := range big {
		big[i] = byte(rng.UintN(256))
	}
	blobs := map[string][]byte{}
	for _, c := range [][]byte{
		{},
		[]byte("hello\n"),
		[]byte("no trailing newline"),
		{0, 1, 2, '\n', 0, 0xff, '\r', '\n', 0},
		[]byte("\n"),
		big,
	} {
		oid := r.hashObject(c)
		if oid != RawOID(c) {
			r.t.Fatalf("git hash-object %s != RawOID %s", oid, RawOID(c))
		}
		blobs[oid] = c
	}
	return blobs
}

func TestSizes(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	blobs := blobFixtures(r)
	var oids []string
	for oid := range blobs {
		oids = append(oids, oid, oid) // duplicates are fine
	}
	sizes, err := r.g.Sizes(t.Context(), oids)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != len(blobs) {
		t.Errorf("Sizes returned %d ids, want %d", len(sizes), len(blobs))
	}
	for oid, c := range blobs {
		if sizes[oid] != int64(len(c)) {
			t.Errorf("Sizes[%s] = %d, want %d", oid, sizes[oid], len(c))
		}
	}

	if sizes, err := r.g.Sizes(t.Context(), nil); err != nil || len(sizes) != 0 {
		t.Errorf("Sizes(nil) = %v, %v", sizes, err)
	}

	r.write("a.txt", "a\n")
	head := r.commit("one")
	tree := r.git("rev-parse", "HEAD^{tree}")
	for _, bad := range [][]string{
		{oids[0], missingOID},
		{head},
		{tree},
		{"HEAD"},
		{oids[0][:12]},
		{oids[0] + "\n" + oids[0]},
		{strings.ToUpper(oids[0])},
	} {
		if _, err := r.g.Sizes(t.Context(), bad); err == nil {
			t.Errorf("Sizes(%q): want error", bad)
		}
	}
	if _, err := r.g.Sizes(t.Context(), []string{missingOID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Sizes(missing) = %v, want ErrNotFound", err)
	}
}

func TestBlobs(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	blobs := blobFixtures(r)
	r.write("a.txt", "a\n")
	head := r.commit("one")
	tree := r.git("rev-parse", "HEAD^{tree}")

	b, err := r.g.OpenBlobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	readAll := func() {
		t.Helper()
		for oid, want := range blobs {
			got, err := b.Read(oid)
			if err != nil {
				t.Fatalf("Read(%s): %v", oid, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("Read(%s) returned %d bytes, want %d", oid, len(got), len(want))
			}
		}
	}
	readAll()
	if _, err := b.Read(missingOID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read(missing) = %v, want ErrNotFound", err)
	}
	readAll() // the stream stays in sync after a missing object
	for _, oid := range []string{head, tree} {
		if _, err := b.Read(oid); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Read(non-blob %s) = %v, want a not-a-blob error", oid, err)
		}
	}
	readAll() // and after skipping a non-blob
	for _, bad := range []string{"", "HEAD", "HEAD:a.txt", head + "\n" + head, head[:10]} {
		if _, err := b.Read(bad); err == nil {
			t.Errorf("Read(%q): want error", bad)
		}
	}
	readAll()

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := b.Read(RawOID([]byte("hello\n"))); !errors.Is(err, errBlobsClosed) {
		t.Errorf("Read after Close = %v, want errBlobsClosed", err)
	}
}

func TestBlobsBrokenStream(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	oid := r.hashObject([]byte("x\n"))
	b, err := r.g.OpenBlobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a desynchronised stream: git's answer is not what Read asked
	// for. Every later Read fails and Close still returns.
	if _, err := b.stdin.Write([]byte(oid + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(missingOID); err == nil {
		t.Fatal("Read on a desynchronised stream: want error")
	}
	if _, err := b.Read(oid); err == nil {
		t.Error("Read after a stream error: want the sticky error")
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close after a stream error: %v", err)
	}
}

func TestOpenBlobsStartFailure(t *testing.T) {
	t.Parallel()
	g := &Git{Bin: "touchmark-no-such-git-binary"}
	if _, err := g.OpenBlobs(t.Context()); err == nil {
		t.Fatal("OpenBlobs with a missing binary: want error")
	}
}

func TestParseObjectHeader(t *testing.T) {
	t.Parallel()
	oid := RawOID(nil)
	if h, err := parseObjectHeader(oid+" blob 12", oid); err != nil || h.size != 12 || h.typ != "blob" {
		t.Errorf("blob header = %+v, %v", h, err)
	}
	if h, err := parseObjectHeader(oid+" missing", oid); err != nil || !h.missing || !errors.Is(h.blobErr(), ErrNotFound) {
		t.Errorf("missing header = %+v, %v", h, err)
	}
	for _, bad := range []string{"", oid, oid + " blob", oid + " blob -1", oid + " blob x", missingOID + " blob 1", oid + " ambiguous"} {
		if _, err := parseObjectHeader(bad, oid); err == nil {
			t.Errorf("parseObjectHeader(%q): want error", bad)
		}
	}
}
