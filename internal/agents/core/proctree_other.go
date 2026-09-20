//go:build !windows

package core

import (
	"os"
	"os/exec"
	"syscall"
)

// prepareTree puts the child in its own process group, so killTree can signal
// the child and everything it starts in one call.
func prepareTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree ends p and every process in its group. See the Windows variant for
// why killing only the direct child is not enough: a wrapper script's real
// CLI would survive and keep the output pipes open.
func killTree(p *os.Process) error {
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return p.Kill()
	}
	return nil
}
