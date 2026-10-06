//go:build unix

package gitx

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureCancel runs the command in a process group of its own and makes
// a cancelled command stop gracefully: the group gets SIGTERM, on which
// every git in it (the command, its remote helper, index-pack) removes its
// lock and temporary files and exits. A command killed with SIGKILL would
// leave shallow.lock behind and fail every later fetch. exec kills the
// command waitDelay later if it still runs.
//
// A group of its own also keeps a terminal's Ctrl-C from killing git
// behind touchmark's back: touchmark cancels its commands itself.
func configureCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
