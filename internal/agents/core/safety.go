package core

import (
	"fmt"
	"strings"
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

// DangerousCommands are command prefixes never allowed, whatever is
// configured. They cover POSIX shells, cmd and PowerShell.
var DangerousCommands = []string{
	// deleting
	"rm", "rmdir", "del", "erase", "rd", "Remove-Item", "shred", "unlink",
	// disks and the system
	"format", "mkfs", "dd", "diskpart", "bcdedit", "fdisk", "mount", "umount",
	"shutdown", "reboot", "halt", "poweroff", "Restart-Computer", "Stop-Computer",
	"reg", "regedit", "Set-ItemProperty", "setx", "sc", "systemctl", "launchctl",
	// privileges and permissions
	"sudo", "su", "runas", "chmod", "chown", "icacls", "takeown", "Set-Acl",
	// processes
	"kill", "pkill", "killall", "taskkill", "Stop-Process",
	// network
	"curl", "wget", "Invoke-WebRequest", "iwr", "Invoke-RestMethod", "irm", "ssh", "scp", "ftp", "nc",
	// version-control history and publishing
	"git push", "git reset", "git clean", "git rebase", "git filter-branch", "git checkout --", "git restore",
	"npm publish", "cargo publish",
	// installing software
	"npm install -g", "pip install", "winget", "choco", "apt", "apt-get", "brew", "Install-Package",
}

// IsDangerousCommand reports whether cmd starts with a refused prefix.
func IsDangerousCommand(cmd string) bool {
	c := strings.ToLower(strings.TrimSpace(cmd))
	for _, d := range DangerousCommands {
		d = strings.ToLower(d)
		if c == d || strings.HasPrefix(c, d+" ") {
			return true
		}
	}
	return false
}

// SafeCommands drops refused commands from a configured allowlist, returning
// what remains and what was dropped.
func SafeCommands(cmds []string) (kept, dropped []string) {
	for _, c := range cmds {
		if IsDangerousCommand(c) {
			dropped = append(dropped, c)
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
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
		names := make([]string, len(g.Builders))
		for i, id := range g.Builders {
			names[i] = string(id)
		}
		b.WriteString("- Apart from reading, you may not run commands. Builds and tests on this task are run by\n")
		fmt.Fprintf(&b, "  %s, whose CLI runs them in a sandbox: ask them (to=<agent>) with the exact commands\n", strings.Join(names, ", "))
		b.WriteString("  and wait for the output. Never call work built or tested until one of them has run it.\n")
	case g.Level.UsesCommandList():
		b.WriteString("- Apart from reading, you may not run commands: no builds, tests or scripts are allowed to you\n")
		b.WriteString("  on this task. If the work needs one, ask the human (to=human) to run it and wait for the result.\n")
	default:
		b.WriteString("- Apart from reading, you may not run commands: no builds, tests or scripts.\n")
		b.WriteString("  If the work needs a build or test, ask the human (to=human) to run it, and do not call the work\n")
		b.WriteString("  built or tested until they answer.\n")
	}
	b.WriteString("- Never delete files you did not create in this task, change system settings or permissions,\n")
	b.WriteString("  kill processes, use the network, install software, or rewrite version-control history.\n")
	b.WriteString("  Such commands are blocked. If the work needs one, ask the human (to=human).\n")
	return b.String()
}
