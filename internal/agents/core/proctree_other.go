//go:build !windows

package core

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// treeEnv names the variable that marks every process of one invocation:
// the CLI is started with it, and whatever it starts inherits it.
const treeEnv = "RITAM_INVOCATION"

// prepareTree puts the child in its own process group, so killTree can signal
// the child and everything it starts in one call, and marks its environment
// (treeEnv), so a process that left the group can still be found.
func prepareTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	cmd.Env = append(cmd.Env, treeEnv+"="+hex.EncodeToString(id[:]))
}

// processTree is one invocation's process group and environment mark.
type processTree struct {
	pid  int
	mark string
}

// startTree records the just-started cmd's group and mark.
func startTree(cmd *exec.Cmd) *processTree {
	t := &processTree{pid: cmd.Process.Pid}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, treeEnv+"=") {
			t.mark = kv
		}
	}
	return t
}

// end ends whatever the invocation left running once the CLI itself has
// exited: a command it put in the background, a process a command started.
// Left running, such a process could change files after the guard checked
// them, or keep working after the turn. The group is killed first; then
// any process still carrying the invocation's mark, which is one that made
// itself a new session and so left the group.
func (t *processTree) end() {
	if t == nil {
		return
	}
	_ = syscall.Kill(-t.pid, syscall.SIGKILL)
	if t.mark == "" {
		return
	}
	for _, pid := range markedProcesses(t.mark) {
		if pid != os.Getpid() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// markedProcesses lists this user's processes whose environment holds mark
// ("RITAM_INVOCATION=<id>").
func markedProcesses(mark string) []int {
	if runtime.GOOS == "linux" {
		var out []int
		dirs, _ := filepath.Glob("/proc/[0-9]*")
		for _, d := range dirs {
			env, err := os.ReadFile(filepath.Join(d, "environ"))
			if err != nil {
				continue // another user's, or gone
			}
			for _, kv := range strings.Split(string(env), "\x00") {
				if kv == mark {
					if pid, err := strconv.Atoi(filepath.Base(d)); err == nil {
						out = append(out, pid)
					}
					break
				}
			}
		}
		return out
	}
	// macOS and the BSDs: ps shows the environment of the user's own
	// processes after the command (-E).
	data, err := exec.Command("ps", "-E", "-ww", "-x", "-o", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var out []int
	for _, l := range strings.Split(string(data), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || !strings.Contains(l, " "+mark) {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			out = append(out, pid)
		}
	}
	return out
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
