package core

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"ritam/internal/domain"
)

// Command safety for agents allowed to write.
//
// An agent that may write edits files inside the workspace and may run what
// the workspace's access level grants it (see Access): nothing by default,
// the listed commands, or any command. Commands that
// could harm the system or anything outside the workspace are refused even
// if a configuration lists them: deleting files, changing system settings
// or permissions, killing processes, reaching the network, and rewriting
// version-control history. Each adapter enforces this with its CLI's own
// mechanism -- Claude Code's tool permissions, Codex's sandbox -- and every
// write-enabled agent is told the rules in its brief.

// DangerousCommands are commands never allowed, whatever is configured.
// They cover POSIX shells, cmd and PowerShell. Each is the words a command
// starts with, where "*" stands for any words in between ("find * -delete"
// is find with -delete anywhere after it). IsDangerousCommand ignores case,
// a program's folder and extension (/bin/rm, rm.exe), wrappers such as env
// and nohup, and git's global options, and checks every command in a chain
// (a && b, a | b, $(b)).
var DangerousCommands = []string{
	// deleting
	"rm", "rmdir", "del", "erase", "rd", "Remove-Item", "shred", "unlink",
	"find * -delete", "find * -exec", "find * -execdir", "find * -ok", "xargs",
	"robocopy * /mir", "robocopy * /purge", "cipher /w", "vssadmin", "wevtutil cl",
	// disks and the system
	"format", "mkfs", "dd", "diskpart", "bcdedit", "fdisk", "mount", "umount",
	"shutdown", "reboot", "halt", "poweroff", "Restart-Computer", "Stop-Computer",
	"reg", "regedit", "Set-ItemProperty", "setx", "sc", "systemctl", "launchctl",
	"schtasks", "at", "crontab", "wmic", "mshta", "rundll32", "regsvr32", "cscript", "wscript",
	"Set-ExecutionPolicy", "netsh", "net user", "net localgroup",
	// privileges and permissions
	"sudo", "su", "runas", "chmod", "chown", "icacls", "takeown", "Set-Acl",
	// processes
	"kill", "pkill", "killall", "taskkill", "Stop-Process",
	// network
	"curl", "wget", "Invoke-WebRequest", "iwr", "Invoke-RestMethod", "irm", "ssh", "scp", "sftp", "ftp",
	"nc", "ncat", "socat", "telnet", "rsync", "certutil", "bitsadmin", "Start-BitsTransfer",
	"git clone", "git fetch", "git pull", "git ls-remote",
	// version-control history, settings and publishing; git's settings name
	// the hooks and programs it runs
	"git push", "git reset", "git clean", "git rebase", "git filter-branch", "git checkout --", "git restore",
	"git branch -d", "git branch -D", "git branch --delete", "git commit --amend", "git stash drop",
	"git stash clear", "git reflog expire", "git update-ref", "git gc", "git prune", "git worktree remove",
	"git config", "git -c", "git remote",
	"npm publish", "cargo publish",
	// installing software, and running packages fetched for the occasion
	"npm install -g", "npm install --global", "npm i -g", "npm i --global", "npx", "pnpm dlx", "yarn dlx", "bunx",
	"pip install", "pip3 install", "python -m pip install", "python3 -m pip install", "py -m pip install",
	"pipx", "uvx", "uv pip install", "uv tool install", "go install", "cargo install", "gem install",
	"dotnet tool install", "winget", "choco", "scoop", "apt", "apt-get", "dnf", "yum", "pacman", "snap", "brew",
	"Install-Package",
	// running a string as code
	"eval", "Invoke-Expression", "iex",
}

// inlineCode are interpreters and shells with the options that run code
// given on their command line (python -c "..."). No list of commands sees
// what that code does, so these are refused too.
var inlineCode = []struct{ programs, flags []string }{
	{[]string{"bash", "sh", "zsh", "dash", "ksh", "fish"}, []string{"-c"}},
	{[]string{"cmd"}, []string{"/c", "/C", "/k", "/K", "//c", "//C", "//k", "//K"}},
	{[]string{"powershell", "pwsh"}, []string{"-c", "-C", "-command", "-Command", "-e", "-ec", "-enc",
		"-encodedcommand", "-EncodedCommand"}},
	{[]string{"python", "python3", "py", "pythonw"}, []string{"-c"}},
	{[]string{"node", "bun"}, []string{"-e", "--eval", "-p", "--print"}},
	{[]string{"deno"}, []string{"eval"}},
	{[]string{"perl"}, []string{"-e", "-E"}},
	{[]string{"ruby"}, []string{"-e"}},
	{[]string{"php"}, []string{"-r"}},
}

// RefusedCommands is every refused command pattern: DangerousCommands, and
// each interpreter with each of its inline-code options. Adapters turn them
// into their CLI's deny rules.
func RefusedCommands() []string {
	out := append([]string(nil), DangerousCommands...)
	for _, ic := range inlineCode {
		for _, p := range ic.programs {
			for _, f := range ic.flags {
				out = append(out, p+" "+f)
			}
		}
	}
	return out
}

// IsDangerousCommand reports whether cmd, or any command chained in it,
// matches a refused pattern.
//
// It filters the command lists people configure, and the deny rules come
// from the same patterns; it is not by itself what keeps an agent safe. A
// command allowed by name can still run whatever it is pointed at -- a build
// runs the project's build files, a test runner its tests -- which is why
// commands run in a sandbox where the CLI has one, and why the settings say
// so where it does not.
func IsDangerousCommand(cmd string) bool {
	for _, words := range commandWords(cmd) {
		for _, r := range RefusedCommands() {
			if matchWords(strings.Fields(r), words) {
				return true
			}
		}
	}
	return false
}

// RunsAnyCode reports whether allowing cmd by name allows any code at all: a
// bare interpreter or shell ("python", "bash"), whose allow rule covers
// every script and every inline program. "python -m pytest" is fine.
func RunsAnyCode(cmd string) bool {
	for _, words := range commandWords(cmd) {
		if len(words) == 1 && isInterpreter(words[0]) {
			return true
		}
	}
	return false
}

func isInterpreter(program string) bool {
	for _, ic := range inlineCode {
		if slices.Contains(ic.programs, program) {
			return true
		}
	}
	return false
}

// commandBreaks split a command line into the commands it runs. Quoting is
// not parsed: a separator inside quotes splits too, which can only find more.
var commandBreaks = strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n", "&", "\n",
	"$(", "\n", "`", "\n", "(", "\n", ")", "\n", `"`, " ", "'", " ")

// wrappers run the command after them.
var wrappers = []string{"env", "command", "builtin", "exec", "nohup", "nice", "time", "timeout", "call", "start", "stdbuf"}

// commandWords splits a command line into its commands, each as words with
// its program normalised: variable assignments and wrappers (with their
// options) dropped, the program lower-case without its folder and
// extension, and git's global options other than -c dropped.
func commandWords(cmd string) [][]string {
	var out [][]string
	for _, part := range strings.Split(commandBreaks.Replace(cmd), "\n") {
		words := strings.Fields(part)
		for len(words) > 0 {
			w := programName(words[0])
			switch {
			case strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "-"):
				words = words[1:] // VAR=value
			case slices.Contains(wrappers, w):
				words = words[1:]
				// env -i, nice -n 5, timeout 30s, start /b
				for len(words) > 0 && (strings.HasPrefix(words[0], "-") || isNumber(words[0]) ||
					strings.HasPrefix(words[0], "/") && len(words[0]) <= 3) {
					words = words[1:]
				}
			default:
				words[0] = w
				if w == "git" {
					words = gitCommand(words)
				}
				out = append(out, words)
				words = nil
			}
		}
	}
	return out
}

// programName is a program without its folder and extension: /bin/rm and
// C:\Windows\System32\cmd.exe are rm and cmd.
func programName(w string) string {
	w = strings.ToLower(w)
	if i := strings.LastIndexAny(w, `/\`); i >= 0 && i < len(w)-1 {
		w = w[i+1:]
	}
	for _, ext := range []string{".exe", ".cmd", ".bat", ".com", ".ps1"} {
		w = strings.TrimSuffix(w, ext)
	}
	return w
}

// gitCommand drops the options git takes before its command: git -C dir
// push is git push. -c stays, since it can set any setting (the hooks and
// programs git runs among them) and is refused itself.
func gitCommand(words []string) []string {
	i := 1
	for i < len(words) {
		switch w := words[i]; {
		case w == "-c":
			return append(words[:1:1], words[i:]...)
		case w == "-C" || w == "--git-dir" || w == "--work-tree" || w == "--namespace" || w == "--exec-path":
			i += 2
		case strings.HasPrefix(w, "-"):
			i++
		default:
			return append(words[:1:1], words[i:]...)
		}
	}
	return words[:1]
}

func isNumber(w string) bool {
	_, err := strconv.ParseFloat(strings.TrimRight(w, "smhd"), 64)
	return err == nil
}

// matchWords reports whether words start with pattern, ignoring case, where
// a "*" in pattern stands for any run of words, none included.
func matchWords(pattern, words []string) bool {
	if len(pattern) == 0 {
		return true
	}
	if pattern[0] == "*" {
		for i := 0; i <= len(words); i++ {
			if matchWords(pattern[1:], words[i:]) {
				return true
			}
		}
		return false
	}
	return len(words) > 0 && strings.EqualFold(words[0], pattern[0]) && matchWords(pattern[1:], words[1:])
}

// SafeCommands drops refused commands from a configured allowlist, returning
// what remains and what was dropped: destructive commands, and bare
// interpreters and shells, which would allow anything.
func SafeCommands(cmds []string) (kept, dropped []string) {
	for _, c := range cmds {
		if IsDangerousCommand(c) || RunsAnyCode(c) {
			dropped = append(dropped, c)
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}

func joinIDs(ids []domain.AgentID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return strings.Join(out, ", ")
}

// WriteRules is the brief's statement of what a write-enabled agent may do
// under its grant.
func WriteRules(workspace string, g Grant) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nWRITE RULES (enforced by the orchestrator; access level %s):\n", g.Level)
	fmt.Fprintf(&b, "- You may create and edit files only inside the project folder %s.\n", workspace)
	b.WriteString("  Never write anywhere else: not temp folders, not your home folder, not system folders.\n")
	// Reading is never restricted: an agent whose only file reader is a shell
	// command (Codex's) must still be able to read the code it is editing.
	b.WriteString("- You may always read and search files, with your file tools or read-only commands\n")
	b.WriteString("  (cat, ls, rg, grep, sed -n, Get-Content, Select-String).\n")
	switch {
	case g.AnyCommand:
		b.WriteString("- You may run any command you need inside the project folder (builds, tests, scripts),\n")
		b.WriteString("  except the blocked ones below.\n")
	case len(g.Commands) > 0:
		fmt.Fprintf(&b, "- Apart from reading, the only commands you may run are: %s.\n", strings.Join(g.Commands, ", "))
	case len(g.Builders) > 0:
		b.WriteString("- Apart from reading, you may not run commands. Builds and tests on this task are run by\n")
		fmt.Fprintf(&b, "  %s: ask them (to=<agent>) with the exact commands and wait for the output.\n", joinIDs(g.Builders))
		b.WriteString("  Never call work built or tested until one of them has run it.\n")
	default:
		b.WriteString("- Apart from reading, you may not run commands: no builds, tests or scripts.\n")
		b.WriteString("  Builds and tests on this task are run by the human: ask the human (to=human) with the exact\n")
		b.WriteString("  commands, and do not call the work built or tested until they answer. The task stays open\n")
		b.WriteString("  until they do.\n")
	}
	if g.BuildDir != "" {
		fmt.Fprintf(&b, "- Build into %s/ only (for example cmake -B %s), never into another agent's build folder,\n", g.BuildDir, g.BuildDir)
		b.WriteString("  and report the exact commands you ran with their output.\n")
		if len(g.Builders) > 0 {
			fmt.Fprintf(&b, "  %s also build%s here; the authority decides who builds what, so do not divide the work yourselves.\n",
				joinIDs(g.Builders), map[bool]string{true: "s", false: ""}[len(g.Builders) == 1])
		}
	}
	if len(g.Assigns) > 1 {
		fmt.Fprintf(&b, "- As the authority, you assign the build and test work: %s may build, each in its own\n", joinIDs(g.Assigns))
		b.WriteString("  build/<agent>/ folder. Tell each one what to build or test; they do not negotiate among themselves.\n")
	}
	b.WriteString("- Never delete files you did not create in this task, change system settings or permissions,\n")
	b.WriteString("  kill processes, use the network, install software, or rewrite version-control history.\n")
	b.WriteString("  Such commands are blocked. If the work needs one, ask the human (to=human).\n")
	return b.String()
}
