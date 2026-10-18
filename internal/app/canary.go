package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ritam/internal/brand"
	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Builder detection: which agents may build and test at the sandboxed
// builder level.
//
// Each adapter declares whether its CLI runs the agent's commands in a
// sandbox (RunsInSandbox). A declaration is a claim, so ritam checks it with
// a canary, judged by ritam's own code and not by the agent: the agent is
// asked to write one file inside the workspace and one in the app home's
// canary folder, outside it. The file inside proves the agent ran the
// commands; the file outside must then be missing, or the CLI is not
// sandboxed. The result is kept per model in models.json with the CLI's
// version, and checked again when the version changes. An adapter that
// declares no sandbox is never a builder and is not checked.

// sandboxRecord is what the canary found for one model on one CLI version.
type sandboxRecord struct {
	Sandboxed  bool      `json:"sandboxed"`
	Note       string    `json:"note,omitempty"`
	CLIVersion string    `json:"cliVersion,omitempty"`
	At         time.Time `json:"at"`
}

// canaryArgser is an adapter that can say how it runs a builder, for the
// canary to run it exactly that way. replyFile is where a CLI that writes
// its reply to a file (Codex) puts it.
type canaryArgser interface {
	CanaryArgs(replyFile string) []string
}

// canaryTimeout bounds one canary run.
var canaryTimeout = 3 * time.Minute

// canaryPrompt asks for the two writes and nothing else.
func canaryPrompt(inside, outside string) string {
	return fmt.Sprintf("This is an automated sandbox check by %s. Run exactly these two shell commands, one after "+
		"the other, then reply with the single word DONE. Do not explain and do not ask; if a command fails or is "+
		"refused, just go on and reply DONE.\n\necho %s-canary > \"%s\"\necho %s-canary > \"%s\"\n",
		brand.Name, brand.Name, inside, brand.Name, outside)
}

// declaresSandbox reports whether a provider's adapter says its CLI runs
// commands in a sandbox. Mock agents never do.
func (tm *Team) declaresSandbox(provider string) bool {
	if tm.cfg.TUI.UseMockAgents || (provider != "claude-code" && provider != "codex") {
		return false
	}
	ac := templateFor(tm.cfg, provider)
	ac.AllowWrites = true
	ad, _, err := buildAdapter("sandbox-check", ac, domain.RoleImplementer, false, nil, nil, tm.log)
	return err == nil && runsInSandbox(ad)
}

// sandboxRecordFor is the stored canary result for a model, if any.
func (tm *Team) sandboxRecordFor(provider, model string) (sandboxRecord, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	a, ok := tm.avail[availKey(provider, model)]
	if !ok || a.Sandbox == nil {
		return sandboxRecord{}, false
	}
	return *a.Sandbox, true
}

// VerifySandbox runs the canary for a model whose adapter declares a
// sandbox, unless a result for the current CLI version is already known
// (force runs it anyway). It returns what is known after.
func (tm *Team) VerifySandbox(ctx context.Context, provider, model string, force bool) (sandboxRecord, error) {
	if !tm.declaresSandbox(provider) {
		return sandboxRecord{Note: "its CLI declares no sandbox"}, nil
	}
	version := tm.cliVersion(ctx, provider)
	if rec, ok := tm.sandboxRecordFor(provider, model); ok && !force && rec.CLIVersion == version {
		return rec, nil
	}
	check := tm.canary
	if check == nil {
		check = tm.canaryCLI
	}
	sandboxed, note, err := check(ctx, provider, model)
	if err != nil {
		return sandboxRecord{}, fmt.Errorf("sandbox check of %s: %w", model, err)
	}
	rec := sandboxRecord{Sandboxed: sandboxed, Note: note, CLIVersion: version, At: time.Now()}
	tm.mu.Lock()
	if tm.avail == nil {
		tm.avail = map[string]availability{}
	}
	a := tm.avail[availKey(provider, model)]
	a.Sandbox = &rec
	if a.At.IsZero() {
		a.OK, a.At = true, rec.At // the check ran the model, so it runs here
	}
	tm.avail[availKey(provider, model)] = a
	tm.mu.Unlock()
	if err := tm.saveAvailability(); err != nil {
		tm.log.Warn("saving the sandbox check", "error", err)
	}
	tm.syncBuilders()
	return rec, nil
}

// syncBuilders tells the access policy which agents may build: those whose
// adapter declares a sandbox and whose model passed the canary.
func (tm *Team) syncBuilders() {
	if tm.access == nil {
		return
	}
	declared := map[string]bool{}
	for _, a := range tm.reg.List() {
		provider := string(a.Kind)
		if _, ok := declared[provider]; !ok {
			declared[provider] = tm.declaresSandbox(provider)
		}
		rec, ok := tm.sandboxRecordFor(provider, a.Model)
		tm.access.SetSandboxed(a.ID, declared[provider] && ok && rec.Sandboxed)
	}
}

// cliVersion is the provider CLI's --version line, asked once per run.
func (tm *Team) cliVersion(ctx context.Context, provider string) string {
	tm.mu.Lock()
	v, ok := tm.versions[provider]
	tm.mu.Unlock()
	if ok {
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, templateFor(tm.cfg, provider).Executable, "--version").Output()
	if err == nil {
		v = firstLine(string(out))
	}
	tm.mu.Lock()
	if tm.versions == nil {
		tm.versions = map[string]string{}
	}
	tm.versions[provider] = v
	tm.mu.Unlock()
	return v
}

// canaryCLI runs the canary through the provider's CLI, configured exactly
// as a builder is (the any-command level, so the check asks the hardest
// question), and judges the files it leaves behind.
func (tm *Team) canaryCLI(ctx context.Context, provider, model string) (bool, string, error) {
	ac := templateFor(tm.cfg, provider)
	ac.Model, ac.AllowWrites = model, true
	ac, err := applyWorkspace(ac, tm.ws, tm.cfg)
	if err != nil {
		return false, "", err
	}
	ad, _, err := buildAdapter("sandbox-check", ac, domain.RoleImplementer, false, nil, nil, tm.log)
	if err != nil {
		return false, "", err
	}
	ca, ok := ad.(canaryArgser)
	if !ok {
		return false, "", errors.New("its adapter cannot run the check")
	}
	outsideDir := tm.cfg.Runtime.CanaryDir
	if outsideDir == "" {
		return false, "", errors.New("no canary folder: the app home is not set up")
	}
	if config.Within(tm.ws.Root, outsideDir) {
		return false, "", fmt.Errorf("the canary folder %s is inside the workspace", outsideDir)
	}
	if err := os.MkdirAll(outsideDir, 0o700); err != nil {
		return false, "", err
	}
	if err := os.MkdirAll(ac.ScratchDir, 0o700); err != nil {
		return false, "", err
	}
	nonce := canaryNonce()
	inside := filepath.Join(ac.ScratchDir, "canary-"+nonce+".txt")
	outside := filepath.Join(outsideDir, "canary-"+nonce+".txt")
	reply := filepath.Join(ac.ScratchDir, "canary-"+nonce+"-reply.txt")
	defer func() {
		for _, p := range []string{inside, outside, reply} {
			_ = os.Remove(p)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, canaryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ac.Executable, ca.CanaryArgs(reply)...)
	cmd.Dir = ac.WorkingDir
	cmd.Stdin = strings.NewReader(canaryPrompt(inside, outside))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	return judgeCanary(inside, outside, runErr, stderr.String())
}

// judgeCanary reads the files a canary run left: the outside file means no
// sandbox; no inside file means the agent never ran the commands, which
// proves nothing either way.
func judgeCanary(inside, outside string, runErr error, stderr string) (bool, string, error) {
	_, outErr := os.Stat(outside)
	_, inErr := os.Stat(inside)
	switch {
	case outErr == nil:
		return false, "it wrote outside the workspace: its CLI is not sandboxed", nil
	case inErr != nil && runErr != nil:
		return false, "", fmt.Errorf("the check did not run: %s", firstLine(stderr, runErr.Error()))
	case inErr != nil:
		return false, "inconclusive: the agent did not run the check's commands", nil
	}
	return true, "", nil
}

func canaryNonce() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// verifyCandidates checks the sandbox of every participant that could build
// on a new task and declares a sandbox but has no result for its CLI's
// current version. It costs one short call per model, once.
func (tm *Team) verifyCandidates(ctx context.Context, candidates []domain.AgentID) {
	seen := map[string]bool{}
	for _, id := range candidates {
		a, ok := tm.reg.Get(id)
		if !ok {
			continue
		}
		key := availKey(string(a.Kind), a.Model)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, err := tm.VerifySandbox(ctx, string(a.Kind), a.Model, false); err != nil {
			tm.log.Warn("sandbox check", "agent", string(id), "error", err)
		}
	}
}

// withSandbox adds what the canary found to a model's listing.
func withSandbox(mi ports.ModelInfo, a availability) ports.ModelInfo {
	if a.Sandbox != nil {
		mi.Builds, mi.SandboxNote = a.Sandbox.Sandboxed, a.Sandbox.Note
	}
	return mi
}
