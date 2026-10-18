package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// AccessConfig is how much the workspace's agents may do beyond editing
// files (see domain.AccessLevel). Like every setting, the project's own
// settings override the global ones, so the human picks it per workspace;
// the settings tab saves it to the project's settings file.
type AccessConfig struct {
	// Level is 0 (files only, the default) to 3 (any command).
	Level int `yaml:"level"`
	// Commands are the command prefixes ("cmake", "ctest", "go test") the
	// sandboxed builders (level 1) or every agent (level 2) may run.
	// Destructive commands are refused whatever is listed.
	Commands []string `yaml:"commands"`
}

// Trusted works out the access actually granted. The settings files of a
// project travel with it -- a clone or a copy carries them -- so what they
// ask for counts only up to what is trusted on this machine: what the global
// settings set, or what the human approved for this project in this folder.
// held describes what was held back, "" when nothing was.
func (c *Config) Trusted() (granted AccessConfig, held string) {
	trusted := c.Runtime.GlobalAccess
	if g := c.Runtime.Grant; g != nil {
		trusted.Level = max(trusted.Level, g.Level)
		trusted.Commands = append(append([]string(nil), trusted.Commands...), g.Commands...)
	}
	granted.Level = min(c.Access.Level, trusted.Level)
	var heldCmds []string
	for _, cmd := range c.Access.Commands {
		if slices.Contains(trusted.Commands, cmd) {
			granted.Commands = append(granted.Commands, cmd)
		} else {
			heldCmds = append(heldCmds, cmd)
		}
	}
	var parts []string
	if granted.Level < c.Access.Level {
		parts = append(parts, fmt.Sprintf("level %d", c.Access.Level))
	}
	if len(heldCmds) > 0 {
		parts = append(parts, "the commands "+strings.Join(heldCmds, ", "))
	}
	if len(parts) == 0 {
		return granted, ""
	}
	return granted, fmt.Sprintf("this project's settings ask for %s, which was never approved for this folder "+
		"on this machine (a cloned or copied project starts at level %d); approve it in :settings",
		strings.Join(parts, " and "), granted.Level)
}

// SaveAccess writes the access settings into the settings file at path,
// creating it if needed. Everything else in the file is kept as it was.
func SaveAccess(path string, ac AccessConfig) error {
	var doc yaml.Node
	data, err := os.ReadFile(path)
	switch {
	case err == nil && len(bytes.TrimSpace(data)) > 0:
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("save access: parse %s: %w", path, err)
		}
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("save access: %w", err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("save access: %s is not a settings mapping", path)
	}
	if ac.Commands == nil {
		ac.Commands = []string{} // written as [], so it overrides a global list
	}
	var value yaml.Node
	if err := value.Encode(ac); err != nil {
		return fmt.Errorf("save access: %w", err)
	}
	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "access" {
			value.HeadComment = root.Content[i+1].HeadComment
			root.Content[i+1] = &value
			replaced = true
		}
	}
	if !replaced {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "access",
			HeadComment: "What agents may run in this workspace (0 files only .. 3 any command). Set from the settings tab."}
		root.Content = append(root.Content, key, &value)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("save access: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("save access: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save access: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o600); err != nil {
		return fmt.Errorf("save access: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save access: %w", err)
	}
	return nil
}
