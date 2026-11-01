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

// processTree is one invocation's process group.
type processTree struct{ pid int }

// startTree records the just-started cmd's group.
func startTree(cmd *exec.Cmd) *processTree { return &processTree{pid: cmd.Process.Pid} }

// end ends whatever the invocation left running in its group once the CLI
// itself has exited: a command it put in the background, a process a
// command started. Left running, such a process could change files after
// the guard checked them, or keep working after the turn. (A process that
// made itself a new session has left the group; only a sandbox holds it.)
func (t *processTree) end() {
	if t != nil {
		_ = syscall.Kill(-t.pid, syscall.SIGKILL)
	}
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
