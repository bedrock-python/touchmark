package gitx

import (
	"slices"
	"sync"
	"sync/atomic"
)

// commandTraces are the functions TraceCommands registered, by
// registration number; count is how many there are, read without the lock
// on every command.
var commandTraces struct {
	mu    sync.Mutex
	next  int
	fns   map[int]func(args, env []string)
	count atomic.Int32
}

// TraceCommands registers fn, which sees the argv (the executable first)
// and the environment of every git command gitx starts, right before it
// starts: hub-side commands and those of target repositories alike, from
// any goroutine of the process. It returns the function that removes fn;
// calling it more than once is fine.
//
// It is a hook for tests: the canary tests check that no
// credential reaches a git argv or environment other than through
// GIT_CONFIG_VALUE_n, and fault injection arms a failure right before a
// given command. Production code never registers one. fn gets copies it
// may keep; it must be safe for concurrent use, and it must not register
// or remove traces itself.
func TraceCommands(fn func(args, env []string)) (remove func()) {
	t := &commandTraces
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fns == nil {
		t.fns = map[int]func(args, env []string){}
	}
	id := t.next
	t.next++
	t.fns[id] = fn
	t.count.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			delete(t.fns, id)
			t.count.Add(-1)
		})
	}
}

// traceCommand passes a command about to start to every registered trace,
// in the order they were registered.
func traceCommand(args, env []string) {
	t := &commandTraces
	if t.count.Load() == 0 {
		return
	}
	t.mu.Lock()
	ids := make([]int, 0, len(t.fns))
	for id := range t.fns {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	fns := make([]func(args, env []string), 0, len(ids))
	for _, id := range ids {
		fns = append(fns, t.fns[id])
	}
	t.mu.Unlock()
	for _, fn := range fns {
		fn(slices.Clone(args), slices.Clone(env))
	}
}
