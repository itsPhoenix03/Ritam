package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A project's review hooks are programs its settings would have ritam run,
// so each runs only once the human approved it on this machine, for that
// project. The approval is of the hook as it stood: its entry and the
// content of the files it names. Change either and it is held again.
//
// Approvals are kept in the app home (hook-approvals.json), which agents
// cannot write: the guard puts back a change made during a turn.

// HookApprovalsFile names the approvals file in the app home.
const HookApprovalsFile = "hook-approvals.json"

// HookApprovals are the approved hooks, by project folder.
type HookApprovals map[string][]string

// HookFingerprint identifies a hook as it stands: its entry, and the content
// of every file among its program and arguments (relative ones are taken
// from the project's folder), so an edited script is a different hook.
func HookFingerprint(h ReviewHook, root string) string {
	sum := sha256.New()
	entry, _ := json.Marshal(h)
	sum.Write(entry)
	for _, f := range HookFiles(h, root) {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		fmt.Fprintf(sum, "\x00%s\x00%d\x00", filepath.ToSlash(f), len(data))
		sum.Write(data)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// HookFiles are the files a hook names that exist: its script, its
// program when given by path. Relative ones are taken from root.
func HookFiles(h ReviewHook, root string) []string {
	var out []string
	for _, a := range h.Run {
		if !strings.ContainsAny(a, `/\`) && !strings.Contains(filepath.Ext(a), ".") {
			continue
		}
		p := a
		if !filepath.IsAbs(p) {
			if root == "" {
				continue
			}
			p = filepath.Join(root, p)
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func approvalKey(root string) string {
	return strings.ToLower(filepath.ToSlash(filepath.Clean(root)))
}

// LoadHookApprovals reads the approvals file; a missing one approves none.
func LoadHookApprovals(path string) (HookApprovals, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return HookApprovals{}, nil
	}
	if err != nil {
		return nil, err
	}
	a := HookApprovals{}
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

// Approved reports whether h, as it stands now, was approved for root.
func (a HookApprovals) Approved(h ReviewHook, root string) bool {
	return slices.Contains(a[approvalKey(root)], HookFingerprint(h, root))
}

// SaveHookApproval records the approval of h, as it stands now, for root.
func SaveHookApproval(path string, h ReviewHook, root string) error {
	a, err := LoadHookApprovals(path)
	if err != nil {
		return err
	}
	key, fp := approvalKey(root), HookFingerprint(h, root)
	if !slices.Contains(a[key], fp) {
		a[key] = append(a[key], fp)
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
