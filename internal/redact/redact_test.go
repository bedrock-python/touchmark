package redact

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// naiveReplace is the reference for Replace: every occurrence of every form,
// occurrences that overlap merged, one Mask per merged run.
func naiveReplace(s string, forms []string) string {
	type interval struct{ start, end int }
	var found []interval
	for i := 0; i < len(s); i++ {
		for _, f := range forms {
			if f != "" && strings.HasPrefix(s[i:], f) {
				found = append(found, interval{i, i + len(f)})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		return found[i].end < found[j].end
	})
	var b strings.Builder
	pos := 0
	for k := 0; k < len(found); {
		start, end := found[k].start, found[k].end
		for k++; k < len(found) && found[k].start < end; k++ {
			end = max(end, found[k].end)
		}
		b.WriteString(s[pos:start])
		b.WriteString(Mask)
		pos = end
	}
	b.WriteString(s[pos:])
	return b.String()
}

// write writes s to w and fails the test on error.
func write(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatal(err)
	}
}

func TestAddForms(t *testing.T) {
	t.Parallel()
	r := New()
	secret := "ab/c+d e%f?g"
	r.Add(secret, "x-access-token", "oauth2")
	want := []string{
		secret,
		url.QueryEscape(secret),
		url.PathEscape(secret),
		base64.StdEncoding.EncodeToString([]byte("x-access-token:" + secret)),
		base64.StdEncoding.EncodeToString([]byte("oauth2:" + secret)),
	}
	slices.Sort(want)
	want = slices.Compact(want)
	if got := r.Forms(); !slices.Equal(got, want) {
		t.Errorf("Forms() = %q, want %q", got, want)
	}

	// Registering again is a no-op; other users add their forms.
	r.Add(secret, "x-access-token")
	if got := r.Forms(); !slices.Equal(got, want) {
		t.Errorf("after a second Add, Forms() = %q, want %q", got, want)
	}
	r.Add(secret, "gitlab-ci-token")
	if got := len(r.Forms()); got != len(want)+1 {
		t.Errorf("after Add with a new user, %d forms, want %d", got, len(want)+1)
	}

	// A plain token has the same raw and escaped forms.
	r2 := New()
	r2.Add("ghs_0123456789abcdef")
	if got := r2.Forms(); !slices.Equal(got, []string{"ghs_0123456789abcdef"}) {
		t.Errorf("Forms() = %q", got)
	}
}

func TestAddIgnoresShortSecrets(t *testing.T) {
	t.Parallel()
	r := New()
	r.Add("")
	r.Add("1234567", "user")
	if got := r.Forms(); len(got) != 0 {
		t.Fatalf("Forms() = %q, want none", got)
	}
	if got := r.Replace("1234567 and user:1234567"); got != "1234567 and user:1234567" {
		t.Errorf("Replace masked a short secret: %q", got)
	}
	r.Add("12345678")
	if got := r.Replace("x12345678y"); got != "x***y" {
		t.Errorf("Replace() = %q, want %q", got, "x***y")
	}
}

func TestNilAndZeroRegistry(t *testing.T) {
	t.Parallel()
	var nilReg *Registry
	nilReg.Add("secret-value-1") // no-op, no panic
	if got := nilReg.Replace("secret-value-1"); got != "secret-value-1" {
		t.Errorf("nil Replace() = %q", got)
	}
	if nilReg.Contains("secret-value-1") || nilReg.Forms() != nil {
		t.Error("nil registry holds forms")
	}
	var buf bytes.Buffer
	w := nilReg.Writer(&buf)
	if _, err := io.WriteString(w, "plain text"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "plain text" {
		t.Errorf("nil Writer wrote %q", buf.String())
	}
	if err := nilReg.GitHubMask(&buf); err != nil {
		t.Fatal(err)
	}

	var zero Registry
	zero.Add("secret-value-2")
	if got := zero.Replace("a secret-value-2 b"); got != "a *** b" {
		t.Errorf("zero Registry Replace() = %q", got)
	}
}

func TestReplace(t *testing.T) {
	t.Parallel()
	r := New()
	r.Add("AAAABBBBBBBB")         // overlaps the next one
	r.Add("BBBBBBBBCCCC")         //
	r.Add("inner-secret")         // nested in the next one
	r.Add("outer-inner-secret-x") //
	r.Add("κωδικός-μυστικό")      // Unicode
	r.Add("tok-12345678", "x-access-token")
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:tok-12345678"))
	tests := []struct{ in, want string }{
		{"", ""},
		{"nothing here", "nothing here"},
		{"x AAAABBBBBBBB y", "x *** y"},
		{"xAAAABBBBBBBBCCCCy", "x***y"},        // overlapping occurrences: one mask
		{"AAAABBBBBBBBAAAABBBBBBBB", "******"}, // touching: two masks
		{"[outer-inner-secret-x]", "[***]"},
		{"[inner-secret]", "[***]"},
		{"κωδικός-μυστικό!", "***!"},
		{"Authorization: Basic " + basic, "Authorization: Basic ***"},
		{"https://h/?t=tok-12345678&x=1", "https://h/?t=***&x=1"},
		{"tok-1234567", "tok-1234567"},
	}
	for _, tt := range tests {
		if got := r.Replace(tt.in); got != tt.want {
			t.Errorf("Replace(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got, want := r.Replace(tt.in), naiveReplace(tt.in, r.Forms()); got != want {
			t.Errorf("Replace(%q) = %q, reference %q", tt.in, got, want)
		}
		if got, want := r.Contains(tt.in), tt.in != tt.want; got != want {
			t.Errorf("Contains(%q) = %v, want %v", tt.in, got, want)
		}
	}
}

func TestWriterSplitsASecret(t *testing.T) {
	t.Parallel()
	r := New()
	secret := "ghs_split-me-please"
	r.Add(secret)
	text := "token=" + secret + "\n"
	for i := 0; i <= len(text); i++ {
		var buf bytes.Buffer
		w := r.Writer(&buf)
		for _, part := range []string{text[:i], text[i:]} {
			n, err := io.WriteString(w, part)
			if err != nil || n != len(part) {
				t.Fatalf("split %d: Write = %d, %v; want %d, nil", i, n, err, len(part))
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if got := buf.String(); got != "token=***\n" {
			t.Errorf("split %d: wrote %q", i, got)
		}
	}
}

func TestWriterHoldsBackOnlyPossibleStarts(t *testing.T) {
	t.Parallel()
	r := New()
	r.Add("-----BEGIN SECRET-----")
	var buf bytes.Buffer
	w := r.Writer(&buf)
	write(t, w, "progress: 1 of 2\n")
	if got := buf.String(); got != "progress: 1 of 2\n" {
		t.Errorf("a line that cannot start a form was held back: %q", got)
	}
	write(t, w, "table |-----")
	if got := buf.String(); got != "progress: 1 of 2\ntable |" {
		t.Errorf("wrote %q, want the possible start held back", got)
	}
	write(t, w, "|\n")
	if got := buf.String(); got != "progress: 1 of 2\ntable |-----|\n" {
		t.Errorf("wrote %q after the start turned out harmless", got)
	}
}

func TestWriterLaterForms(t *testing.T) {
	t.Parallel()
	r := New()
	var buf bytes.Buffer
	w := r.Writer(&buf)
	write(t, w, "before: late-secret-1 ")
	r.Add("late-secret-1")
	write(t, w, "after: late-secret-1\n")
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "before: late-secret-1 after: ***\n"; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}

	// Forms added while a possible start is held back apply to it.
	r2 := New()
	r2.Add("abcdefgh")
	buf.Reset()
	w2 := r2.Writer(&buf)
	write(t, w2, "xx abcd")
	r2.Add("abcdXYZW")
	write(t, w2, "XYZW yy")
	w2.Close()
	if got, want := buf.String(), "xx *** yy"; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

func TestWriterFlushEndsASegment(t *testing.T) {
	t.Parallel()
	r := New()
	r.Add("secret-value")
	var buf bytes.Buffer
	w := r.Writer(&buf)
	write(t, w, "a secret-")
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "a secret-" {
		t.Errorf("Flush wrote %q", got)
	}
	write(t, w, "value secret-value")
	w.Close()
	if got, want := buf.String(), "a secret-value ***"; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

type failWriter struct {
	n   int
	err error
}

func (f *failWriter) Write(p []byte) (int, error) { return f.n, f.err }

func TestWriterErrors(t *testing.T) {
	t.Parallel()
	r := New()
	boom := errors.New("boom")
	w := r.Writer(&failWriter{err: boom})
	if n, err := w.Write([]byte("text")); !errors.Is(err, boom) || n != 0 {
		t.Errorf("Write = %d, %v; want 0, boom", n, err)
	}
	if _, err := w.Write([]byte("more")); !errors.Is(err, boom) {
		t.Errorf("second Write error = %v, want the first error", err)
	}
	if err := w.Close(); !errors.Is(err, boom) {
		t.Errorf("Close = %v, want the first error", err)
	}

	short := r.Writer(&failWriter{n: 1})
	if _, err := short.Write([]byte("text")); !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("short Write error = %v, want io.ErrShortWrite", err)
	}
}

func TestWriterConcurrent(t *testing.T) {
	t.Parallel()
	r := New()
	r.Add("concurrent-secret")
	var buf bytes.Buffer
	w := r.Writer(&buf)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				fmt.Fprintf(w, "worker %d line %d concurrent-secret\n", i, j)
				if j == 10 {
					r.Add(fmt.Sprintf("added-by-%d-xyz", i))
				}
			}
		}()
	}
	wg.Wait()
	w.Close()
	if strings.Contains(buf.String(), "concurrent-secret") {
		t.Error("output contains the secret")
	}
	if got := strings.Count(buf.String(), "\n"); got != 400 {
		t.Errorf("wrote %d lines, want 400", got)
	}
}

func TestGitHubMask(t *testing.T) {
	t.Parallel()
	r := New()
	key := "-----BEGIN KEY-----\r\nMIIEowIBAAKCAQEA0123456789\r\nabc\r\n-----END KEY-----\r\n"
	r.Add(key)
	r.Add("pct%secret+x")
	var buf bytes.Buffer
	if err := r.GitHubMask(&buf); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		"::add-mask::-----BEGIN KEY-----\n",
		"::add-mask::MIIEowIBAAKCAQEA0123456789\n",
		"::add-mask::-----END KEY-----\n",
		"::add-mask::pct%25secret+x\n",       // raw form, escaped
		"::add-mask::pct%2525secret%252Bx\n", // QueryEscape form, escaped
	} {
		if !strings.Contains(got, want) {
			t.Errorf("GitHubMask output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "::add-mask::abc\n") {
		t.Error("GitHubMask wrote a line shorter than MinLen")
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !strings.HasPrefix(line, "::add-mask::") || strings.Contains(line, "\r") {
			t.Errorf("bad command line %q", line)
		}
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if !slices.IsSorted(lines) || len(slices.Compact(slices.Clone(lines))) != len(lines) {
		t.Errorf("commands are not sorted and unique:\n%s", got)
	}
}

// alphabet has ASCII, URL-special characters, a newline and multi-byte
// runes, but no '*': a form that contains '*' could be rebuilt around a
// Mask.
var alphabet = []rune("abcAB09+/=%:_-. ?&@\nλé€😀")

func randString(rng *rand.Rand, minBytes, maxBytes int) string {
	n := minBytes + rng.IntN(maxBytes-minBytes+1)
	var b strings.Builder
	for b.Len() < n {
		b.WriteRune(alphabet[rng.IntN(len(alphabet))])
	}
	return b.String()
}

// randomCase builds a registry of related secrets and a text full of their
// forms, fragments and overlaps.
func randomCase(rng *rand.Rand) (*Registry, string) {
	r := New()
	var secrets []string
	for range 1 + rng.IntN(4) {
		var s string
		switch {
		case len(secrets) > 0 && rng.IntN(3) == 0:
			// Starts with the tail of an earlier secret: occurrences overlap.
			prev := secrets[rng.IntN(len(secrets))]
			s = prev[len(prev)/2:] + randString(rng, 1, 12)
		case len(secrets) > 0 && rng.IntN(3) == 0:
			// Contains an earlier secret.
			s = randString(rng, 0, 4) + secrets[rng.IntN(len(secrets))] + randString(rng, 1, 4)
		default:
			s = randString(rng, MinLen, 30)
		}
		secrets = append(secrets, s)
		var users []string
		if rng.IntN(2) == 0 {
			users = append(users, "x-access-token")
		}
		r.Add(s, users...)
		if len(secrets)%2 == 1 {
			// Build what is registered so far: later secrets go to
			// automata of their own, which then merge.
			r.Contains("")
		}
	}
	forms := r.Forms()
	var text strings.Builder
	for range rng.IntN(12) {
		switch rng.IntN(5) {
		case 0:
			text.WriteString(randString(rng, 0, 20))
		case 1:
			text.WriteString(forms[rng.IntN(len(forms))])
		case 2:
			f := forms[rng.IntN(len(forms))]
			text.WriteString(f[:rng.IntN(len(f))]) // a fragment
		case 3:
			// Two secrets glued so that their occurrences overlap, when
			// the second starts with the tail of the first.
			a, b := secrets[rng.IntN(len(secrets))], secrets[rng.IntN(len(secrets))]
			k := longestOverlap(a, b)
			text.WriteString(a + b[k:])
		default:
			text.WriteString(secrets[rng.IntN(len(secrets))])
		}
	}
	return r, text.String()
}

// longestOverlap returns the length of the longest proper suffix of a that
// is a prefix of b.
func longestOverlap(a, b string) int {
	for k := min(len(a)-1, len(b)); k > 0; k-- {
		if strings.HasSuffix(a, b[:k]) {
			return k
		}
	}
	return 0
}

// randomSplit cuts s into pieces at random byte offsets, including empty
// pieces and cuts inside runes.
func randomSplit(rng *rand.Rand, s string) []string {
	var parts []string
	for len(s) > 0 {
		n := rng.IntN(len(s) + 1)
		if rng.IntN(4) == 0 {
			n = rng.IntN(min(len(s), 3) + 1)
		}
		parts = append(parts, s[:n])
		s = s[n:]
	}
	return append(parts, "")
}

// checkWriter writes parts through a Writer of r and checks the output
// against the reference; it returns a description of the first failure.
func checkWriter(r *Registry, text string, parts []string) error {
	forms := r.Forms()
	longest := 0
	for _, f := range forms {
		longest = max(longest, len(f))
	}
	var buf bytes.Buffer
	w := r.Writer(&buf)
	for _, p := range parts {
		n, err := w.Write([]byte(p))
		if err != nil || n != len(p) {
			return fmt.Errorf("Write(%q) = %d, %w; want %d, nil", p, n, err, len(p))
		}
		if held := len(w.m.held()); held > max(longest-1, 0) {
			return fmt.Errorf("held back %d bytes, longest form is %d", held, longest)
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	got := buf.String()
	want := naiveReplace(text, forms)
	if got != want {
		return fmt.Errorf("wrote %q, reference %q (forms %q, parts %q)", got, want, forms, parts)
	}
	if rep := r.Replace(text); rep != want {
		return fmt.Errorf("Replace = %q, reference %q (forms %q)", rep, want, forms)
	}
	for _, f := range forms {
		if !strings.Contains(f, "*") && strings.Contains(got, f) {
			return fmt.Errorf("output %q contains form %q", got, f)
		}
	}
	if r.Contains(got) && !slices.ContainsFunc(forms, func(f string) bool { return strings.Contains(f, "*") }) {
		return fmt.Errorf("Contains(output %q) = true", got)
	}
	return nil
}

func TestWriterProperty(t *testing.T) {
	t.Parallel()
	for seed := range uint64(3000) {
		rng := rand.New(rand.NewPCG(seed, 0x7e57))
		r, text := randomCase(rng)
		if err := checkWriter(r, text, randomSplit(rng, text)); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		// One byte at a time.
		if err := checkWriter(r, text, strings.Split(text, "")); err != nil {
			t.Fatalf("seed %d, byte by byte: %v", seed, err)
		}
	}
}

func TestWriterUnicodeSplitInsideRunes(t *testing.T) {
	t.Parallel()
	r := New()
	secret := "μυστικό😀κωδικός"
	r.Add(secret)
	text := "κλειδί: " + secret + " ok"
	for i := 0; i <= len(text); i++ {
		if utf8.RuneStart(text[min(i, len(text)-1)]) {
			continue // only cuts inside runes here
		}
		if err := checkWriter(r, text, []string{text[:i], text[i:]}); err != nil {
			t.Fatalf("cut at %d: %v", i, err)
		}
	}
}

// TestWriterLongHeldPrefix writes a long possible start of a form byte by
// byte: each write must cost O(1) amortized, not O(bytes held), or this
// test takes minutes.
func TestWriterLongHeldPrefix(t *testing.T) {
	t.Parallel()
	r := New()
	prefix := strings.Repeat("a", 64<<10)
	r.Add(prefix + "b")
	text := prefix[:60<<10] + strings.Repeat("a", 1<<20) + "c"
	var buf bytes.Buffer
	w := r.Writer(&buf)
	for i := 0; i < len(text)-1; i++ {
		if _, err := w.Write([]byte{text[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if held := len(w.m.held()); held != 64<<10 {
		t.Errorf("held %d bytes, want %d", held, 64<<10)
	}
	write(t, w, text[len(text)-1:])
	w.Close()
	if buf.String() != text {
		t.Error("output differs from the input, which holds no form")
	}
}

func FuzzWriter(f *testing.F) {
	f.Add("AAAABBBBBBBB", "BBBBBBBBCCCC", "xAAAABBBBBBBBCCCCy", uint64(1))
	f.Add("secret-value", "secret-value-longer", "a secret-value-longer secret-value b", uint64(2))
	f.Add("κωδικός-μυστικό", "μυστικό-κλειδί😀", "κωδικός-μυστικό-κλειδί😀", uint64(3))
	f.Fuzz(func(t *testing.T, s1, s2, text string, seed uint64) {
		if len(s1) > 64 || len(s2) > 64 || len(text) > 1<<10 {
			t.Skip("the reference is quadratic")
		}
		r := New()
		r.Add(s1, "x-access-token")
		r.Add(s2)
		rng := rand.New(rand.NewPCG(seed, seed>>1))
		if err := checkWriter(r, text, randomSplit(rng, text)); err != nil {
			t.Fatal(err)
		}
	})
}

// TestManySecrets: a run registers a token per target and masks between
// two of them. Every form stays masked, the forms live in O(log n)
// automata, and each form was built into one O(log n) times: never a
// rebuild of all of them per secret.
func TestManySecrets(t *testing.T) {
	t.Parallel()
	r := New()
	const n = 1000
	var tokens []string
	for i := range n {
		tok := fmt.Sprintf("ghs_%0520d", i*7919)
		tokens = append(tokens, tok)
		r.Add(tok, "x-access-token")
		if r.Contains("no secret here") {
			t.Fatal("Contains of plain text")
		}
		if !r.Contains("token " + tok + ".") {
			t.Fatalf("token %d is not masked right after Add", i)
		}
	}
	for _, i := range []int{0, 1, n / 2, n - 1} {
		text := "a " + tokens[i] + " b " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tokens[i]))
		if got := r.Replace(text); got != "a *** b ***" {
			t.Errorf("token %d: Replace = %.60q", i, got)
		}
	}
	r.mu.Lock()
	levels := len(r.levels)
	total := 0
	for i, lv := range r.levels {
		total += len(lv.forms)
		if i > 0 && len(r.levels[i-1].forms) < 2*len(lv.forms) {
			t.Errorf("level %d holds %d forms, the one before %d: not half", i, len(lv.forms), len(r.levels[i-1].forms))
		}
	}
	r.mu.Unlock()
	if bound := 2 + int(math.Log2(float64(total))); levels > bound {
		t.Errorf("%d levels for %d forms, want at most %d", levels, total, bound)
	}
	if total != len(r.Forms()) {
		t.Errorf("the levels hold %d forms, the registry %d", total, len(r.Forms()))
	}
}

// TestOnAdd: the hook learns the forms each Add registers, before Add
// returns, and nothing for a secret registered again or too short.
func TestOnAdd(t *testing.T) {
	t.Parallel()
	r := New()
	var got [][]string
	r.OnAdd(func(forms []string) { got = append(got, forms) })
	r.Add("first-secret-1", "x-access-token")
	r.Add("first-secret-1", "x-access-token")
	r.Add("short")
	r.Add("first-secret-1", "oauth2")
	if len(got) != 2 || len(got[0]) != 2 || len(got[1]) != 1 { // the escaped forms equal the raw one
		t.Fatalf("hook calls %q", got)
	}
	var buf bytes.Buffer
	if err := GitHubMaskForms(&buf, got[0]); err != nil || !strings.Contains(buf.String(), "::add-mask::first-secret-1\n") {
		t.Errorf("GitHubMaskForms: %q, %v", buf.String(), err)
	}
	r.OnAdd(nil)
	r.Add("second-secret")
	if len(got) != 2 {
		t.Errorf("a removed hook was called: %q", got)
	}
	var nilReg *Registry
	nilReg.OnAdd(func([]string) { t.Error("called on a nil registry") })
	nilReg.Add("third-secret")
}

func BenchmarkAddContains(b *testing.B) {
	for b.Loop() {
		r := New()
		for i := range 500 {
			r.Add(fmt.Sprintf("ghs_%0520d", i), "x-access-token")
			r.Contains("report line")
		}
	}
}

func BenchmarkWriter(b *testing.B) {
	r := New()
	for i := range 20 {
		r.Add(fmt.Sprintf("ghs_%040d", i), "x-access-token")
	}
	line := []byte("resolve gh:acme/service-42: opened #123 (3 writes) - see https://github.com/acme/service-42/pull/123\n")
	w := r.Writer(io.Discard)
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		if _, err := w.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}
