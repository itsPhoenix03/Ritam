package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// A settings file is read strictly: a key ritam does not know is an error,
// never ignored. A mistyped key that were ignored would leave the default in
// force while the user believes it changed -- `maxDailyCostUsd: 5` would keep
// the $50 daily limit -- and for limits that is worse than not starting.

// decodeStrict decodes one settings file's data into c, keeping whatever the
// file leaves out. An unknown key is an error naming the file, the key and
// the nearest valid key when one is close.
func decodeStrict(path string, data []byte, c *Config) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return nil // an empty file, or one holding only comments
	}
	if err := checkKeys(doc.Content[0], reflect.TypeOf(*c), ""); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	// The walk above names the key and suggests one; the decoder's own check
	// stays on as well, so nothing the walk might miss slips through.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// checkKeys walks node against the type it decodes into and reports the first
// key that type does not have. prefix is the dotted path to node.
func checkKeys(node *yaml.Node, t reflect.Type, prefix string) error {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return nil // a wrong shape is the decoder's to report
		}
		fields := yamlFields(t)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Value == "<<" { // a merge key brings in more of the same mapping
				if err := checkKeys(value, t, prefix); err != nil {
					return err
				}
				continue
			}
			f, ok := fields[key.Value]
			if !ok {
				return unknownKey(key, prefix, fields)
			}
			if err := checkKeys(value, f.Type, join(prefix, key.Value)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := checkKeys(node.Content[i+1], t.Elem(), join(prefix, node.Content[i].Value)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return nil
		}
		for i, item := range node.Content {
			if err := checkKeys(item, t.Elem(), fmt.Sprintf("%s[%d]", prefix, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// yamlFields maps each key a struct accepts to its field.
func yamlFields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if strings.Contains(opts, "inline") && f.Type.Kind() == reflect.Struct {
			for k, v := range yamlFields(f.Type) {
				out[k] = v
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name) // yaml.v3's own default
		}
		out[name] = f
	}
	return out
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// unknownKey is the error for a key the settings do not have.
func unknownKey(key *yaml.Node, prefix string, fields map[string]reflect.StructField) error {
	msg := fmt.Sprintf("line %d: unknown setting %s", key.Line, join(prefix, key.Value))
	if near := nearestKey(key.Value, fields); near != "" {
		msg += fmt.Sprintf(" (did you mean %s?)", near)
	} else if elsewhere := keyElsewhere(key.Value); elsewhere != "" {
		msg += fmt.Sprintf(" (it belongs under %s)", elsewhere)
	}
	return errors.New(msg)
}

// nearestKey is the valid key closest to key: one that differs only in
// case, or else one a few typing mistakes away. "" when none is close.
func nearestKey(key string, fields map[string]reflect.StructField) string {
	best, bestDist := "", 0
	for name := range fields {
		if strings.EqualFold(name, key) {
			return name
		}
		d := editDistance(strings.ToLower(key), strings.ToLower(name))
		if best == "" || d < bestDist || (d == bestDist && name < best) {
			best, bestDist = name, d
		}
	}
	// A third of the key's length is close enough to be a typo rather than
	// a different word.
	if best != "" && bestDist <= max(1, len(key)/3) {
		return best
	}
	return ""
}

// keyElsewhere finds a section key belongs in when it was written at the
// wrong level, such as idleTimeout at the top instead of under orchestrator.
// "" when it is in no section, or in more than one.
func keyElsewhere(key string) string {
	var found []string
	for section, f := range yamlFields(reflect.TypeOf(Config{})) {
		t := f.Type
		if t.Kind() != reflect.Struct {
			continue
		}
		for name := range yamlFields(t) {
			if strings.EqualFold(name, key) {
				found = append(found, section+"."+name)
			}
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
