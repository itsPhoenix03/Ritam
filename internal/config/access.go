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
	// AllowUnsandboxed lets agents whose CLI has no verified sandbox
	// (Claude Code on native Windows) run commands at levels 2 and 3,
	// directly on this machine. Off, the default, they never run commands:
	// they ask an agent that has a sandbox, or the human. Only the global
	// settings may turn it on; a project's settings file cannot.
	AllowUnsandboxed bool `yaml:"allowUnsandboxed,omitempty"`
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
	if ac.Commands == nil {
		ac.Commands = []string{} // written as [], so it overrides a global list
	}
	return saveSection(path, "access", ac,
		"What agents may run in this workspace (0 files only .. 3 any command). Set from the settings tab.", "save access")
}

// saveSection writes one top-level section of the settings file at path,
// creating the file if needed and keeping everything else, the section's
// own comment included, as it was. what prefixes the errors.
func saveSection(path, section string, v any, comment, what string) error {
	doc, root, err := readSettingsDoc(path, what)
	if err != nil {
		return err
	}
	var value yaml.Node
	if err := value.Encode(v); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	setMapKey(root, section, &value, comment)
	return writeSettingsDoc(path, doc, what)
}

// saveKey sets one key inside a top-level section of the settings file at
// path, keeping the rest of the section and of the file as it was.
func saveKey(path, section, key string, v any, comment, what string) error {
	doc, root, err := readSettingsDoc(path, what)
	if err != nil {
		return err
	}
	var sec *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == section && root.Content[i+1].Kind == yaml.MappingNode {
			sec = root.Content[i+1]
		}
	}
	if sec == nil {
		sec = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMapKey(root, section, sec, comment)
	}
	var value yaml.Node
	if err := value.Encode(v); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	setMapKey(sec, key, &value, "")
	return writeSettingsDoc(path, doc, what)
}

// setMapKey puts value under key in a mapping, keeping an existing key's
// comment.
func setMapKey(m *yaml.Node, key string, value *yaml.Node, comment string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			value.HeadComment = m.Content[i+1].HeadComment
			m.Content[i+1] = value
			return
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key, HeadComment: comment}
	m.Content = append(m.Content, k, value)
}

// readSettingsDoc reads the settings file at path as a YAML document, an
// empty mapping when it does not exist yet.
func readSettingsDoc(path, what string) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	data, err := os.ReadFile(path)
	switch {
	case err == nil && len(bytes.TrimSpace(data)) > 0:
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, nil, fmt.Errorf("%s: parse %s: %w", what, path, err)
		}
	case err != nil && !os.IsNotExist(err):
		return nil, nil, fmt.Errorf("%s: %w", what, err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("%s: %s is not a settings mapping", what, path)
	}
	return &doc, root, nil
}

// writeSettingsDoc writes a settings document to path, atomically.
func writeSettingsDoc(path string, doc *yaml.Node, what string) error {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o600); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}
