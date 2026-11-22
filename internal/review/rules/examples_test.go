package rules

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"ritam/internal/review/facts"
)

// The example machines: the rules file's examples run with the workspace
// as the folder, the build folder at {workspace}/build, and the home folder
// outside the workspace.
func exampleEnv(os string) facts.Env {
	switch os {
	case "windows":
		return facts.Env{OS: "windows", Workspace: `C:\Users\me\proj`, Build: `C:\Users\me\proj\build`,
			Home: `C:\Users\me`, Temp: []string{`C:\Users\me\AppData\Local\Temp`},
			RitamHome: `C:\Users\me\AppData\Local\ritam`,
			Vars: map[string]string{"APPDATA": `C:\Users\me\AppData\Roaming`, "LOCALAPPDATA": `C:\Users\me\AppData\Local`,
				"PROGRAMDATA": `C:\ProgramData`, "WINDIR": `C:\Windows`, "PROGRAMFILES": `C:\Program Files`,
				"TEMP": `C:\Users\me\AppData\Local\Temp`, "USERPROFILE": `C:\Users\me`}}
	case "macos":
		return facts.Env{OS: "macos", Workspace: "/Users/me/proj", Build: "/Users/me/proj/build", Home: "/Users/me",
			Temp: []string{"/tmp", "/private/tmp"}, RitamHome: "/Users/me/.ritam"}
	}
	return facts.Env{OS: "linux", Workspace: "/home/me/proj", Build: "/home/me/proj/build", Home: "/home/me",
		Temp: []string{"/tmp"}, RitamHome: "/home/me/.ritam"}
}

// exampleOS picks the machine an example runs on: the rule's first OS, else
// Windows for PowerShell and cmd, else Linux.
func exampleOS(r *Rule, e Example) string {
	if len(r.OS) > 0 {
		return r.OS[0]
	}
	if e.Kind == "ps" || e.Kind == "cmd" {
		return "windows"
	}
	return "linux"
}

func exampleCall(e Example, env facts.Env) *facts.Call {
	tool := func(name string, input map[string]string) *facts.Call {
		data, _ := json.Marshal(input)
		return facts.Build(facts.Input{Provider: "claude-code", Tool: name, ToolInput: data}, env)
	}
	switch e.Kind {
	case "bash":
		return facts.Shell(facts.Bash, e.Text, "", env)
	case "ps":
		return facts.Shell(facts.PowerShell, e.Text, "", env)
	case "cmd":
		return facts.Shell(facts.Cmd, e.Text, "", env)
	case "write":
		return tool("Write", map[string]string{"file_path": e.Text, "content": e.Content})
	case "read":
		return tool("Read", map[string]string{"file_path": e.Text})
	case "edit":
		return tool("Edit", map[string]string{"file_path": e.Text, "old_string": "x", "new_string": e.Content})
	case "url":
		return tool("WebFetch", map[string]string{"url": e.Text, "prompt": "read"})
	case "output":
		return facts.Output(e.Text, env)
	}
	return nil
}

// knownMisses lists the examples the parser does not get right yet, one
// per line as "rule-id hit|miss example". The test fails when one of them
// starts passing (remove it) as much as when a new one fails, so the list
// stays the truth.
const knownMissesFile = "testdata/known-misses.txt"

func loadKnownMisses(t *testing.T) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(knownMissesFile)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return out
}

// TestEveryRuleExample: every "hit" example fires its rule and no "miss"
// example does, except the known misses.
func TestEveryRuleExample(t *testing.T) {
	s, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	known := loadKnownMisses(t)
	var failed []string
	total := 0
	for _, r := range s.Rules {
		if r.On == OnSequence || r.Disabled() != "" {
			continue
		}
		check := func(e Example, want bool) {
			total++
			env := exampleEnv(exampleOS(r, e))
			call := exampleCall(e, env)
			if call == nil {
				t.Errorf("%s: unknown example kind %s", r.ID, e.Kind)
				return
			}
			if s.Fires(r, call, env) != want {
				kind := "hit"
				if !want {
					kind = "miss"
				}
				failed = append(failed, fmt.Sprintf("%s %s %s", r.ID, kind, e))
			}
		}
		for _, e := range r.Examples.Hit {
			check(e, true)
		}
		for _, e := range r.Examples.Miss {
			check(e, false)
		}
	}
	sort.Strings(failed)
	var fresh []string
	got := map[string]bool{}
	for _, f := range failed {
		got[f] = true
		if !known[f] {
			fresh = append(fresh, f)
		}
	}
	var fixed []string
	for k := range known {
		if !got[k] {
			fixed = append(fixed, k)
		}
	}
	sort.Strings(fixed)
	t.Logf("%d of %d examples right, %d known misses", total-len(failed), total, len(known))
	if os.Getenv("RITAM_WRITE_KNOWN_MISSES") == "1" {
		_ = os.MkdirAll("testdata", 0o755)
		body := "# Rule examples the parser does not get right yet (see TestEveryRuleExample).\n" +
			"# Regenerate: RITAM_WRITE_KNOWN_MISSES=1 go test -run TestEveryRuleExample ./internal/review/rules/\n" +
			strings.Join(failed, "\n") + "\n"
		if err := os.WriteFile(knownMissesFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, f := range fresh {
		t.Errorf("example wrong: %s", f)
	}
	for _, f := range fixed {
		t.Errorf("example now right, remove it from %s: %s", knownMissesFile, f)
	}
}
