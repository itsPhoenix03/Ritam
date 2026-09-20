//go:build windows

package core

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// prepareTree readies cmd so killTree can later end it with everything it
// started. Windows needs no preparation: taskkill walks the parent/child
// relationship itself.
func prepareTree(*exec.Cmd) {}

// killTree ends p and every process it started.
//
// Killing only the direct child is not enough on Windows. A CLI installed by
// npm is a `.cmd` shim, so the direct child is cmd.exe and the real CLI is a
// node grandchild that inherits stdout and stderr. Killing cmd.exe left node
// running, holding the pipes open: the caller blocked until node finished on
// its own (29 seconds in the run that exposed this) and node kept working --
// and spending -- after the task it belonged to had ended.
//
// taskkill is addressed by PID, never by image name. Killing by name would
// take out every process with that name on the machine, including unrelated
// sessions of the same CLI.
func killTree(p *os.Process) error {
	tk := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	tk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := tk.Run(); err != nil {
		// taskkill missing or the tree already gone: still make sure the
		// direct child dies.
		return p.Kill()
	}
	return nil
}
