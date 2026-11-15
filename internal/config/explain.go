package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Explaining the effective settings: with defaults, a global file, two
// project files and settings held back, "why is this value what it is?" has
// no easy answer from the files alone. `ritam config check` prints it.

// Layer is one settings file in the order LoadLayers read it.
type Layer struct {
	// Name says whose file it is: "global" or "project".
	Name string
	Path string
}

// Setting is one effective setting and, when it differs from the default
// or was held back, where it came from.
type Setting struct {
	// Key is the dotted path, as written in a settings file.
	Key   string
	Value string
	// Source is "" for a default value; otherwise the layer that set it
	// ("project (path)"), "set at start" for a path ritam works out itself,
	// a note that the project's value was held back, or the reason given in
	// adjusted.
	Source string
}

// Explain lists every effective setting in cfg, in the order a settings file
// would hold them. layers are the files cfg was loaded from, held what
// RestrictProject held back, and adjusted gives reasons for values changed
// after loading (the real-agent idle floor), keyed by setting.
func Explain(cfg *Config, layers []Layer, held []string, adjusted map[string]string) ([]Setting, error) {
	effective, order, err := flatten(cfg)
	if err != nil {
		return nil, err
	}
	defaults, _, err := flatten(Default())
	if err != nil {
		return nil, err
	}
	sets := make([]map[string]bool, len(layers))
	for i, l := range layers {
		if sets[i], err = keysSet(l.Path); err != nil {
			return nil, err
		}
	}

	out := make([]Setting, 0, len(order))
	for _, key := range order {
		s := Setting{Key: key, Value: effective[key]}
		def, hasDefault := defaults[key]
		differs := !hasDefault || def != s.Value
		switch {
		case adjusted[key] != "":
			s.Source = adjusted[key]
		case differs:
			s.Source = "set at start"
			for i := len(layers) - 1; i >= 0; i-- {
				if setsKey(sets[i], key) {
					s.Source = layers[i].Name + " (" + layers[i].Path + ")"
					break
				}
			}
		}
		if isHeld(held, key) {
			note := "the project's value was held back"
			if s.Source == "" {
				s.Source = note
			} else {
				s.Source += "; " + note
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// flatten turns a configuration into dotted keys and printable values, and
// the keys in file order. A list is one value.
func flatten(cfg *Config) (map[string]string, []string, error) {
	var root yaml.Node
	if err := root.Encode(cfg); err != nil {
		return nil, nil, fmt.Errorf("explain settings: %w", err)
	}
	values := map[string]string{}
	var order []string
	var walk func(n *yaml.Node, prefix string)
	walk = func(n *yaml.Node, prefix string) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				walk(n.Content[i+1], join(prefix, n.Content[i].Value))
			}
		case yaml.SequenceNode:
			items := make([]string, 0, len(n.Content))
			for _, item := range n.Content {
				items = append(items, scalarText(item))
			}
			values[prefix] = "[" + strings.Join(items, ", ") + "]"
			order = append(order, prefix)
		default:
			values[prefix] = scalarText(n)
			order = append(order, prefix)
		}
	}
	walk(&root, "")
	return values, order, nil
}

func scalarText(n *yaml.Node) string {
	switch {
	case n.Kind != yaml.ScalarNode:
		data, _ := yaml.Marshal(n)
		return strings.TrimSpace(string(data))
	case n.Tag == "!!null":
		return "(unset)"
	case n.Value == "":
		return `""`
	}
	return n.Value
}

// keysSet reads which settings a file sets: every value it gives, and every
// agent entry, since an entry replaces the whole agent. A file that does not
// exist sets nothing.
func keysSet(path string) (map[string]bool, error) {
	set := map[string]bool{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || path == "" {
		return set, nil
	}
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	var walk func(n *yaml.Node, prefix string)
	walk = func(n *yaml.Node, prefix string) {
		for n.Kind == yaml.AliasNode && n.Alias != nil {
			n = n.Alias
		}
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, prefix)
			}
		case yaml.MappingNode:
			if strings.Count(prefix, ".") == 1 && strings.HasPrefix(prefix, "agents.") {
				set[prefix] = true
			}
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i].Value
				if key == "<<" {
					walk(n.Content[i+1], prefix)
					continue
				}
				walk(n.Content[i+1], join(prefix, key))
			}
		default:
			set[prefix] = true
		}
	}
	walk(&doc, "")
	return set, nil
}

// setsKey reports whether a file's set includes key, itself or as part of
// a value or agent entry it gives whole.
func setsKey(set map[string]bool, key string) bool {
	for k := key; ; {
		if set[k] {
			return true
		}
		i := strings.LastIndex(k, ".")
		if i < 0 {
			return false
		}
		k = k[:i]
	}
}

// isHeld reports whether a setting was held back. Held names are keys, or a
// key followed by what was held (": bash", " (outside the project)"), and a
// section name ("backups") covers the section.
func isHeld(held []string, key string) bool {
	for _, h := range held {
		if i := strings.IndexAny(h, ": ("); i >= 0 {
			h = h[:i]
		}
		if h == key || strings.HasPrefix(key, h+".") {
			return true
		}
	}
	return false
}
