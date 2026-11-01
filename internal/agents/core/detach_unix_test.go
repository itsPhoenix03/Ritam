//go:build !windows

package core

import (
	"os/exec"
	"syscall"
)

// detach makes c a new session, leaving the invocation's process group the
// way a process that wants to outlive it would.
func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
