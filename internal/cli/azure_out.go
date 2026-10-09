package cli

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// The Azure Pipelines agent reads a logging command, ##vso[area.action
// …]…, anywhere in a line a step prints, and runs it: it sets variables for
// the later steps, uploads files, prepends to PATH
// (https://learn.microsoft.com/en-us/azure/devops/pipelines/scripts/logging-commands).
// touchmark prints what the platforms and the targets say (error messages,
// paths of files in the targets), so on Azure Pipelines (TF_BUILD=True)
// everything it prints goes through commandGuard, which breaks every
// "##vso[" so that the agent sees no command in it.

// vsoPrefix starts an Azure Pipelines logging command, and vsoBroken is
// what commandGuard prints instead.
const (
	vsoPrefix = "##vso["
	vsoBroken = "##vso ["
)

// commandGuard is a writer that breaks every vsoPrefix written through it,
// also one split across writes: it holds back a tail that may begin one
// until the next write, or Flush. It is safe for concurrent use.
type commandGuard struct {
	mu   sync.Mutex
	w    io.Writer
	tail []byte
}

func newCommandGuard(w io.Writer) *commandGuard { return &commandGuard{w: w} }

// Write writes p, with every vsoPrefix broken; it reports len(p) written
// when the underlying writer took all it was given.
func (g *commandGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	buf := append(g.tail, p...)
	buf = bytes.ReplaceAll(buf, []byte(vsoPrefix), []byte(vsoBroken))
	keep := heldBack(buf)
	g.tail = append([]byte(nil), buf[len(buf)-keep:]...)
	if _, err := g.w.Write(buf[:len(buf)-keep]); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush writes what Write held back.
func (g *commandGuard) Flush() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.tail) == 0 {
		return nil
	}
	_, err := g.w.Write(g.tail)
	g.tail = nil
	return err
}

// heldBack returns the length of the longest suffix of buf that is a proper
// prefix of vsoPrefix: the next write could complete it.
func heldBack(buf []byte) int {
	for n := min(len(vsoPrefix)-1, len(buf)); n > 0; n-- {
		if strings.HasPrefix(vsoPrefix, string(buf[len(buf)-n:])) {
			return n
		}
	}
	return 0
}
