//go:build windows

package core

import "os/exec"

// detach does nothing on Windows: the job object holds every descendant.
func detach(*exec.Cmd) {}
