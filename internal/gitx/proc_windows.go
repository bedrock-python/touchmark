//go:build windows

package gitx

import (
	"os/exec"
	"syscall"
	"unsafe"
)

// processQueryLimited is PROCESS_QUERY_LIMITED_INFORMATION, which package
// syscall does not name.
const processQueryLimited = 0x1000

// treeWait bounds, in milliseconds, how long killTree waits for each
// process it terminated to be gone.
const treeWait = 5000

// configureCancel makes a cancelled command stop together with every
// process it started. The git.exe found on PATH is often Git for Windows'
// launcher (Git\cmd\git.exe), which runs the real git as a child, and git
// runs remote helpers and index-pack as children in turn. TerminateProcess
// on the launcher alone would leave them running: holding the command's
// output pipes (so Wait lasts until waitDelay), lock files such as
// shallow.lock, and, for a push, the request that may still land. So the
// command's descendants are terminated and waited for, then the command.
//
// TerminateProcess skips git's cleanup: the lock files a killed fetch
// leaves are removed before the next fetch (removeStaleLocks).
func configureCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		killTree(uint32(cmd.Process.Pid))
		return cmd.Process.Kill()
	}
}

// killTree terminates the running descendants of the process root (not
// root itself) and waits for them, at most treeWait each.
//
// Descendants are found through a snapshot of the processes and their
// parent ids. Windows keeps a parent id after the parent exits, and ids are
// reused, so a process counts as a child only when it was created after
// its parent: an older process whose parent id was reused is someone
// else's. Processes started while the tree is being terminated may escape.
func killTree(root uint32) {
	rootCreated, ok := processCreated(root)
	if !ok {
		return
	}
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer func() { _ = syscall.CloseHandle(snap) }()
	children := map[uint32][]uint32{}
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		if e.ProcessID != e.ParentProcessID {
			children[e.ParentProcessID] = append(children[e.ParentProcessID], e.ProcessID)
		}
	}
	type proc struct {
		pid     uint32
		created int64
	}
	var victims []syscall.Handle
	seen := map[uint32]bool{root: true}
	for queue := []proc{{root, rootCreated}}; len(queue) > 0; queue = queue[1:] {
		parent := queue[0]
		for _, pid := range children[parent.pid] {
			if seen[pid] {
				continue
			}
			seen[pid] = true
			h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE|syscall.SYNCHRONIZE|processQueryLimited, false, pid)
			if err != nil {
				continue
			}
			created, ok := handleCreated(h)
			if !ok || created < parent.created {
				_ = syscall.CloseHandle(h)
				continue
			}
			victims = append(victims, h)
			queue = append(queue, proc{pid, created})
		}
	}
	// The deepest first, so that no parent outlives its children to start
	// another one.
	for i := len(victims) - 1; i >= 0; i-- {
		_ = syscall.TerminateProcess(victims[i], 1)
	}
	for _, h := range victims {
		_, _ = syscall.WaitForSingleObject(h, treeWait)
		_ = syscall.CloseHandle(h)
	}
}

// processCreated returns the creation time of the process pid.
func processCreated(pid uint32) (int64, bool) {
	h, err := syscall.OpenProcess(processQueryLimited, false, pid)
	if err != nil {
		return 0, false
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	return handleCreated(h)
}

// handleCreated returns the creation time of the process of h, in
// nanoseconds.
func handleCreated(h syscall.Handle) (int64, bool) {
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, false
	}
	return created.Nanoseconds(), true
}
