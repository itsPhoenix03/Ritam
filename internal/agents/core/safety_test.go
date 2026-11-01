package core

import (
	"strings"
	"testing"

	"ritam/internal/domain"
)

func TestDestructiveCommandsAreRefusedWhateverIsConfigured(t *testing.T) {
	kept, dropped := SafeCommands([]string{"go test", "go build ./...", "rm -rf build", "git push origin main",
		"Remove-Item x", "curl http://x", "npm test", "RM file"})
	if strings.Join(kept, "|") != "go test|go build ./...|npm test" {
		t.Errorf("kept %v", kept)
	}
	if len(dropped) != 5 {
		t.Errorf("dropped %v, want the five destructive ones", dropped)
	}
	// A prefix match only: "rmate" is not "rm".
	if IsDangerousCommand("rmate open") {
		t.Error("a command merely starting with the same letters is not refused")
	}
}

// Every way of reaching a refused command that a security review found
// getting past a first-word match.
func TestRefusedCommandsAreFoundThroughWrappersInterpretersAndChains(t *testing.T) {
	for _, cmd := range []string{
		`bash -c "rm -rf src"`, `sh -c 'rm -rf src'`, `cmd /c del /s /q src`, `cmd //c del x`,
		`powershell -Command Remove-Item -Recurse src`, `pwsh -c "ri src"`,
		`python -c "import shutil; shutil.rmtree('src')"`, `python3 -c "import os"`, `node -e "require('fs').rmSync('src')"`,
		`perl -e "unlink glob '*'"`, `ruby -e "File.delete('x')"`,
		`find . -name '*.go' -delete`, `find . -type f -exec rm {} ;`, `ls | xargs rm`,
		`/bin/rm -rf src`, `C:\Windows\System32\cmd.exe /c del x`, `rm.exe x`, `env rm x`, `env FOO=1 rm x`,
		`nohup rm x`, `nice -n 5 rm x`, `timeout 30 rm x`, `FOO=bar rm x`,
		`go build ./... && rm -rf src`, `go test ./... ; curl https://example.com`, `echo $(rm -rf src)`, "echo `rm x`",
		`git -C . push origin main`, `git --no-pager reset --hard`, `git commit --amend -m x`, `git branch -D main`,
		`git -c core.hooksPath=x commit`, `git config core.fsmonitor x`, `git remote add x y`, `git clone https://x`,
		`certutil -urlcache -f https://x y`, `bitsadmin /transfer x`, `schtasks /create /tn x`, `wmic process call create x`,
		`npx some-package`, `pip3 install x`, `python -m pip install x`, `go install example.com/x@latest`,
		`robocopy empty src /MIR`, `Invoke-Expression x`, `iex x`,
	} {
		if !IsDangerousCommand(cmd) {
			t.Errorf("%q is not refused", cmd)
		}
	}
}

func TestOrdinaryBuildAndTestCommandsAreNotRefused(t *testing.T) {
	for _, cmd := range []string{
		"go test ./...", "go build ./... 2>&1", "go vet ./...", "cmake -B build/a", "cmake --build build/a",
		"ctest --test-dir build/a", "npm test", "npm run build", "python -m pytest -q", "python tests/run.py",
		"git status", "git diff", "git log --oneline -5", "git -C sub status", "git add -A", "git commit -m msg",
		"ls -la", "grep -rn x .", "find . -name '*.go'", "make -j4", "cargo test", "node build.js",
		"rmate open", "formatter --check", "clang-format -i x.cpp", "date", "atlas migrate",
	} {
		if IsDangerousCommand(cmd) {
			t.Errorf("%q is refused", cmd)
		}
	}
}

func TestABareInterpreterCannotBeAllowedByName(t *testing.T) {
	kept, dropped := SafeCommands([]string{"python", "bash", "node", "python -m pytest", "bash scripts/build.sh", "go test"})
	if strings.Join(kept, "|") != "python -m pytest|bash scripts/build.sh|go test" {
		t.Errorf("kept %v", kept)
	}
	if strings.Join(dropped, "|") != "python|bash|node" {
		t.Errorf("dropped %v: a bare interpreter would run anything", dropped)
	}
}

func TestTheWriteRulesNameTheFolderAndTheCommands(t *testing.T) {
	r := WriteRules(`C:\z\project-orch`, Grant{Level: domain.AccessBuildAndTest, Commands: []string{"go test"}})
	for _, want := range []string{`C:\z\project-orch`, "not temp folders", "go test", "blocked", "Build and test"} {
		if !strings.Contains(r, want) {
			t.Errorf("rules missing %q:\n%s", want, r)
		}
	}
	if r := WriteRules("/p", Grant{}); !strings.Contains(r, "may not run commands") || !strings.Contains(r, "always read and search files") {
		t.Error("with no allowed commands the agent runs nothing but may still read")
	}
}
