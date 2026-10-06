// Package redact masks secrets in everything touchmark prints or writes.
//
// A Registry holds every token and key touchmark reads or mints, in every
// form it may appear in: raw, URL-encoded, and base64 of "user:secret" as
// in HTTP Basic headers. Writers wrap stdout, stderr and report files and
// replace any registered form with "***", even when it is split across
// writes.
//
// Every occurrence of every form is masked, overlapping ones included:
// occurrences that overlap become one Mask, so no byte of a form that occurs
// in the text is ever written. Matching is byte-wise (Aho-Corasick
// automata), so the cost is linear in the text and grows only with the
// logarithm of the number of forms.
//
// A run registers a token per target as it mints them (a GitHub App), and
// masks between two mints: the forms are kept in a few automata
// of geometrically growing size, merged like the digits of a binary
// counter, so that a new secret costs an automaton of its own forms and,
// amortized, O(log n) rebuilds of each form, never a rebuild of all of
// them (thousands of targets would otherwise spend minutes rebuilding).
package redact

import (
	"encoding/base64"
	"io"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
)

// MinLen is the shortest secret registered. Shorter strings would mask
// ordinary text; Add ignores them.
const MinLen = 8

// Mask replaces a secret in output.
const Mask = "***"

// Registry is safe for concurrent use. The zero value is an empty registry,
// and a nil *Registry masks nothing (Add on it is a no-op).
type Registry struct {
	mu    sync.Mutex
	forms map[string]struct{}
	// levels are automata over disjoint sets of forms, each over at least
	// twice as many forms as the next; pending are the forms added since
	// the last build. m matches all of them; nil until needed and after Add
	// changed the forms.
	levels  []level
	pending []string
	m       *matcher
	// onAdd, when set, is told the forms each Add registers.
	onAdd func(forms []string)
}

// level is one automaton of a Registry and the forms it matches.
type level struct {
	forms []string // sorted
	a     *automaton
}

// New returns an empty registry.
func New() *Registry { return &Registry{forms: map[string]struct{}{}} }

// Add registers secret and its derived forms: the raw string, its
// url.QueryEscape and url.PathEscape forms, and for each user in
// basicUsers the standard base64 of "user:secret" (git's Basic header,
// e.g. users "x-access-token" or "oauth2"). Registering the same secret
// twice is a no-op; registering it again with other users adds their
// forms. Secrets shorter than MinLen bytes are ignored. The OnAdd hook
// learns the new forms before Add returns.
func (r *Registry) Add(secret string, basicUsers ...string) {
	if r == nil || len(secret) < MinLen {
		return
	}
	forms := []string{secret, url.QueryEscape(secret), url.PathEscape(secret)}
	for _, user := range basicUsers {
		forms = append(forms, base64.StdEncoding.EncodeToString([]byte(user+":"+secret)))
	}
	r.mu.Lock()
	if r.forms == nil {
		r.forms = map[string]struct{}{}
	}
	var added []string
	for _, f := range forms {
		if _, ok := r.forms[f]; !ok {
			r.forms[f] = struct{}{}
			r.pending = append(r.pending, f)
			added = append(added, f)
			r.m = nil
		}
	}
	hook := r.onAdd
	r.mu.Unlock()
	if hook != nil && len(added) > 0 {
		hook(added)
	}
}

// OnAdd sets a hook that every later Add calls, synchronously and before it
// returns, with the forms it registered (none: no call): the CLI prints
// "::add-mask::" commands on GitHub Actions with it, so that a secret the
// drivers mint during the run is masked by the runner before its first
// use. The hook must not write through a Writer of r.
// Nil removes it.
func (r *Registry) OnAdd(hook func(forms []string)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onAdd = hook
}

// Replace returns s with every registered form replaced by Mask, longest
// forms first: an occurrence inside a longer one is masked with it, and
// occurrences that overlap are masked as one. Occurrences that only touch
// are masked separately. s is returned as is when it holds no form.
func (r *Registry) Replace(s string) string {
	a := r.matcher()
	if !a.contains(s) {
		return s
	}
	var m masker
	m.use(a)
	out := m.feed(make([]byte, 0, len(s)), []byte(s))
	return string(m.flush(out))
}

// Contains reports whether s holds any registered form. The engine refuses
// to write a PR body, comment, commit message or branch name that does
// (failed:secret-exposure).
func (r *Registry) Contains(s string) bool { return r.matcher().contains(s) }

// Forms returns the registered forms, sorted.
func (r *Registry) Forms() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return sortedForms(r.forms)
}

// matcher returns the matcher of the current forms, building it when they
// changed: the pending forms become an automaton of their own, and the
// last two levels merge while the one before holds fewer than twice the
// forms of the last, so the levels shrink at least by half each and number
// O(log n).
func (r *Registry) matcher() *matcher {
	if r == nil {
		return emptyMatcher
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m != nil {
		return r.m
	}
	if len(r.pending) > 0 {
		forms := slices.Sorted(slices.Values(r.pending))
		r.levels = append(r.levels, level{forms: forms, a: build(forms)})
		r.pending = nil
		for n := len(r.levels); n >= 2 && len(r.levels[n-2].forms) < 2*len(r.levels[n-1].forms); n = len(r.levels) {
			merged := mergeSorted(r.levels[n-2].forms, r.levels[n-1].forms)
			r.levels = append(r.levels[:n-2], level{forms: merged, a: build(merged)})
		}
	}
	m := &matcher{autos: make([]*automaton, len(r.levels))}
	for i, lv := range r.levels {
		m.autos[i] = lv.a
	}
	r.m = m
	return m
}

// mergeSorted merges two sorted lists of distinct forms.
func mergeSorted(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] < b[j] {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}

// matcher matches the forms of several automata as one: the longest form
// that ends at a byte is the longest any of them reports, and the longest
// suffix that may start a form the deepest state of any.
type matcher struct{ autos []*automaton }

var emptyMatcher = &matcher{}

// contains reports whether s holds a form of any automaton.
func (m *matcher) contains(s string) bool {
	for _, a := range m.autos {
		if a.contains(s) {
			return true
		}
	}
	return false
}

// step advances states over c and returns the length of the longest form
// that ends there, 0 if none.
func (m *matcher) step(states []int32, c byte) int32 {
	longest := int32(0)
	for i, a := range m.autos {
		s := a.step(states[i], c)
		states[i] = s
		longest = max(longest, a.match[s])
	}
	return longest
}

// depth returns the length of the longest suffix read so far that may
// start a form.
func (m *matcher) depth(states []int32) int32 {
	d := int32(0)
	for i, a := range m.autos {
		d = max(d, a.depth[states[i]])
	}
	return d
}

func sortedForms(forms map[string]struct{}) []string {
	out := make([]string, 0, len(forms))
	for f := range forms {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Writer returns a writer that masks registered forms before writing to w.
// It holds back at most (longest form - 1) bytes between writes so a secret
// split across two writes is still masked; Close (or Flush) writes the
// rest. Forms added after the writer was created apply to later writes.
//
// Only bytes that may begin a registered form are held back: text whose
// tail cannot start a form (a line ending in a newline, typically) is
// written at once. What a Writer writes between two flushes equals Replace
// of what it was given, however the input was split. A nil registry gives
// a writer that masks nothing.
func (r *Registry) Writer(w io.Writer) *Writer { return &Writer{r: r, w: w} }

// Writer is a masking writer. It is safe for concurrent use; a failed write
// to the underlying writer is returned by every later call.
type Writer struct {
	mu  sync.Mutex
	r   *Registry
	w   io.Writer
	m   masker
	buf []byte
	err error
}

// Write masks p and writes what can no longer be part of a form. It
// returns len(p) unless the underlying writer fails.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.m.use(w.r.matcher())
	w.buf = w.m.feed(w.buf[:0], p)
	if err := w.writeBuf(); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush writes the held-back bytes (masked). A form split across a Flush is
// not masked: Flush ends a segment as Close does.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.m.use(w.r.matcher())
	w.buf = w.m.flush(w.buf[:0])
	return w.writeBuf()
}

// Close flushes. It does not close the underlying writer.
func (w *Writer) Close() error { return w.Flush() }

// maxKeep is the largest buffer a Writer keeps between writes.
const maxKeep = 64 << 10

// writeBuf writes the masked output and records a failure.
func (w *Writer) writeBuf() error {
	if len(w.buf) > 0 {
		n, err := w.w.Write(w.buf)
		if err == nil && n < len(w.buf) {
			err = io.ErrShortWrite
		}
		if err != nil {
			w.err = err
		}
	}
	if cap(w.buf) > maxKeep {
		w.buf = nil
	}
	return w.err
}

// GitHubMask writes one "::add-mask::<form>" workflow command per form to w,
// for GitHub Actions to mask the forms in its own log rendering. Forms with
// a newline are written line by line.
//
// Lines shorter than MinLen and duplicates are left out, and the values are
// sorted. '%', CR and LF are escaped as workflow commands require. w must
// not be a Writer of r: it would mask the values themselves.
func (r *Registry) GitHubMask(w io.Writer) error { return GitHubMaskForms(w, r.Forms()) }

// GitHubMaskForms writes the "::add-mask::" commands of forms to w, as
// GitHubMask does for a registry's: with Registry.OnAdd, the forms a run
// registers after its start.
func GitHubMaskForms(w io.Writer, forms []string) error {
	seen := map[string]bool{}
	var values []string
	for _, f := range forms {
		lines := []string{f}
		if strings.Contains(f, "\n") {
			lines = strings.Split(f, "\n")
			for i, l := range lines {
				lines[i] = strings.TrimSuffix(l, "\r")
			}
		}
		for _, l := range lines {
			if len(l) >= MinLen && !seen[l] {
				seen[l] = true
				values = append(values, l)
			}
		}
	}
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	var b strings.Builder
	for _, v := range values {
		b.WriteString("::add-mask::")
		b.WriteString(commandEscaper.Replace(v))
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// commandEscaper escapes the data of a GitHub Actions workflow command; the
// runner reverses it.
var commandEscaper = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")

// span is a run of masked stream bytes [start, end), in absolute offsets.
type span struct{ start, end int64 }

// masker masks a stream fed in pieces.
//
// Offsets are absolute positions in the stream since the masker started.
// Bytes before base are written (as is, or under a Mask); buf[head:] holds
// the bytes [base, pos). A match is a whole occurrence of a form that ends
// at or before pos; matches that overlap merge into one span. The automaton
// state tells the longest suffix of the stream that may start a form
// (depth): no future match can start before pos - depth, so everything
// before that is final.
type masker struct {
	a *matcher
	// states are the states of a's automata.
	states []int32
	buf    []byte
	head   int
	base   int64
	pos    int64
	// open is the end of the last span written as a Mask. A match that
	// starts before it extends that span without writing another Mask.
	open int64
	// spans[first:] are matched spans not written yet, in order, merged,
	// each starting at or after max(base, open).
	spans []span
	first int
}

// held returns the bytes not written yet.
func (m *masker) held() []byte { return m.buf[m.head:] }

// bytes returns the held bytes [from, to).
func (m *masker) bytes(from, to int64) []byte {
	return m.buf[m.head+int(from-m.base) : m.head+int(to-m.base)]
}

// use switches to matcher a (the registry changed). The held bytes are
// matched again from the automata's start; forms that would have started
// in bytes already written cannot be masked any more.
func (m *masker) use(a *matcher) {
	if m.a == a {
		return
	}
	m.a = a
	m.reset()
	m.spans, m.first = m.spans[:0], 0
	end := m.base
	for _, c := range m.held() {
		end++
		m.step(c, end)
	}
}

// reset puts every automaton back at its start.
func (m *masker) reset() {
	m.states = slices.Grow(m.states[:0], len(m.a.autos))[:len(m.a.autos)]
	clear(m.states)
}

// step advances the automata over c, the stream byte ending at offset end,
// and records the longest form that ends there.
func (m *masker) step(c byte, end int64) {
	if n := m.a.step(m.states, c); n > 0 {
		m.add(end-int64(n), end)
	}
}

// feed masks p, appends to out what is final and returns it.
func (m *masker) feed(out, p []byte) []byte {
	m.buf = append(m.buf, p...)
	for _, c := range p {
		m.pos++
		m.step(c, m.pos)
	}
	return m.emit(out, m.pos-int64(m.a.depth(m.states)))
}

// flush appends everything held to out and starts a new segment.
func (m *masker) flush(out []byte) []byte {
	out = m.emit(out, m.pos)
	m.reset()
	return out
}

// add records the match [start, end); end is the current position.
func (m *masker) add(start, end int64) {
	if start < m.open {
		// It overlaps the span already written: that span now reaches end
		// and swallows every held span, all of which end by end.
		m.open = end
		m.spans, m.first = m.spans[:0], 0
		return
	}
	// Held spans that end after start overlap the match; they are a suffix
	// of the list, since spans are ordered and do not overlap.
	i := len(m.spans)
	for i > m.first && m.spans[i-1].end > start {
		i--
	}
	if i < len(m.spans) {
		start = min(start, m.spans[i].start)
	}
	m.spans = append(m.spans[:i], span{start, end})
}

// emit appends to out the bytes before cut and every span that starts at or
// before cut (as a Mask), then drops them. A span that starts by cut is
// final: later matches start at or after cut, so they can only extend it.
// cut may lie before base (inside the last Mask written): a held span that
// only touches that Mask is not final then, since a later match may start
// inside the Mask and join the two.
func (m *masker) emit(out []byte, cut int64) []byte {
	next := max(m.base, m.open) // first byte not written or masked yet
	for m.first < len(m.spans) && m.spans[m.first].start <= cut {
		s := m.spans[m.first]
		m.first++
		out = append(out, m.bytes(next, s.start)...)
		out = append(out, Mask...)
		m.open, next = s.end, s.end
	}
	if m.first == len(m.spans) {
		m.spans, m.first = m.spans[:0], 0
	}
	if cut > next {
		out = append(out, m.bytes(next, cut)...)
		next = cut
	}
	m.drop(next)
	return out
}

// drop forgets the held bytes before offset next. The buffer is compacted
// once more than half of it is dead, so a write costs O(len(p)) amortized
// however long the held bytes are.
func (m *masker) drop(next int64) {
	m.head += int(next - m.base)
	m.base = next
	switch {
	case m.head == len(m.buf):
		m.buf, m.head = m.buf[:0], 0
	case m.head > len(m.buf)/2:
		n := copy(m.buf, m.buf[m.head:])
		m.buf, m.head = m.buf[:n], 0
	}
	if cap(m.buf) > maxKeep && len(m.buf)-m.head <= maxKeep/4 {
		m.buf, m.head = slices.Clone(m.buf[m.head:]), 0
	}
}

// automaton is an Aho-Corasick automaton over the bytes of the forms. Node
// 0 is the root; a node stands for the prefix of some form that leads to
// it. It is immutable once built.
type automaton struct {
	// root holds the root's transitions; 0 means staying at the root.
	root [256]int32
	// first[v]:first[v+1] are node v's edges in edges, sorted by byte.
	first []int32
	edges []edge
	// fail[v] is the node of the longest proper suffix of v's prefix that
	// is a prefix of some form.
	fail []int32
	// depth[v] is the length of v's prefix.
	depth []int32
	// match[v] is the length of the longest form that is a suffix of v's
	// prefix, 0 if none.
	match []int32
}

type edge struct {
	c  byte
	to int32
}

// build returns the automaton for forms.
//
// With the forms sorted, the trie grows along the previous form's path, and
// the children of every node appear in byte order; the edges are then laid
// out per node without sorting or maps.
func build(forms []string) *automaton {
	if !slices.IsSorted(forms) {
		forms = slices.Sorted(slices.Values(forms))
	}
	forms = slices.Compact(slices.Clone(forms))

	parent := []int32{0} // parent[v] and label[v] form the edge into v
	label := []byte{0}
	depth := []int32{0}
	term := []bool{false}
	path := []int32{0} // path[d] is the node at depth d of the previous form
	prev := ""
	for _, f := range forms {
		common := 0
		for common < len(prev) && common < len(f) && prev[common] == f[common] {
			common++
		}
		path = path[:common+1]
		v := path[common]
		for i := common; i < len(f); i++ {
			t := int32(len(parent))
			parent = append(parent, v)
			label = append(label, f[i])
			depth = append(depth, int32(i+1))
			term = append(term, false)
			path = append(path, t)
			v = t
		}
		term[v] = len(f) > 0
		prev = f
	}

	n := len(parent)
	a := &automaton{
		first: make([]int32, n+1),
		edges: make([]edge, n-1),
		fail:  make([]int32, n),
		depth: depth,
		match: make([]int32, n),
	}
	for v := 1; v < n; v++ {
		a.first[parent[v]+1]++
	}
	for v := 1; v <= n; v++ {
		a.first[v] += a.first[v-1]
	}
	fill := slices.Clone(a.first[:n])
	for v := 1; v < n; v++ {
		p := parent[v]
		a.edges[fill[p]] = edge{label[v], int32(v)}
		fill[p]++
	}
	for _, e := range a.children(0) {
		a.root[e.c] = e.to
	}

	// Breadth first, so fail links and matches of shallower nodes are known.
	queue := make([]int32, 0, n)
	for _, e := range a.children(0) {
		queue = append(queue, e.to)
	}
	for i := 0; i < len(queue); i++ {
		v := queue[i]
		if term[v] {
			a.match[v] = a.depth[v]
		} else {
			a.match[v] = a.match[a.fail[v]]
		}
		for _, e := range a.children(v) {
			a.fail[e.to] = a.step(a.fail[v], e.c)
			queue = append(queue, e.to)
		}
	}
	return a
}

func (a *automaton) children(v int32) []edge { return a.edges[a.first[v]:a.first[v+1]] }

// child returns the child of v (not the root) over c, or 0.
func (a *automaton) child(v int32, c byte) int32 {
	es := a.children(v)
	if len(es) > 8 {
		i := sort.Search(len(es), func(i int) bool { return es[i].c >= c })
		if i < len(es) && es[i].c == c {
			return es[i].to
		}
		return 0
	}
	for _, e := range es {
		if e.c == c {
			return e.to
		}
	}
	return 0
}

// step returns the state after reading c in state v.
func (a *automaton) step(v int32, c byte) int32 {
	for v != 0 {
		if t := a.child(v, c); t != 0 {
			return t
		}
		v = a.fail[v]
	}
	return a.root[c]
}

// contains reports whether s holds a form.
func (a *automaton) contains(s string) bool {
	if len(a.match) == 1 {
		return false // no forms
	}
	v := int32(0)
	for i := 0; i < len(s); i++ {
		v = a.step(v, s[i])
		if a.match[v] > 0 {
			return true
		}
	}
	return false
}
