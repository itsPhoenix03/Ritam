//go:build windows

package core

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// prepareTree readies cmd so its whole tree can be ended later: it starts
// suspended, so startTree can put it in a job object before it runs, and so
// before it can start anything that would escape the job.
func prepareTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// processTree is one invocation's process and everything it starts: a job
// object that ends every process still in it when closed.
type processTree struct{ job windows.Handle }

// startTree puts the just-started, suspended cmd in a job object and lets
// it run. A process that cannot be resumed is killed: it would otherwise
// wait, suspended, until the timeout.
func startTree(cmd *exec.Cmd) *processTree {
	t := &processTree{}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		return t
	}
	defer windows.CloseHandle(h)
	if job, err := windows.CreateJobObject(nil, nil); err == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
			BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
				LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
			},
		}
		_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
		if err == nil {
			err = windows.AssignProcessToJobObject(job, h)
		}
		if err == nil {
			t.job = job
		} else {
			_ = windows.CloseHandle(job)
		}
	}
	if status, _, _ := ntResumeProcess.Call(uintptr(h)); status != 0 {
		_ = cmd.Process.Kill()
	}
	return t
}

// end ends whatever the invocation left running once the CLI itself has
// exited: a command it put in the background, a process a command started.
// Left running, such a process could change files after the guard checked
// them, or keep working -- and spending -- after the turn.
func (t *processTree) end() {
	if t != nil && t.job != 0 {
		_ = windows.CloseHandle(t.job)
		t.job = 0
	}
}

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
