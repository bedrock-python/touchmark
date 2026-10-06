//go:build !unix && !windows

package gitx

import "os/exec"

// configureCancel keeps exec's default: a cancelled command is killed.
func configureCancel(*exec.Cmd) {}
