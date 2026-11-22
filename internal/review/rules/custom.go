package rules

import (
	"fmt"
	"strings"

	"ritam/internal/review/facts"
)

// Custom are rules from the human's own settings (review.rules): command
// patterns to deny or ask about, and paths to protect. They only add
// findings, so they can make the review stricter, never looser.
type Custom struct {
	Deny, Ask, Protect []string
}

// customCategory is the category of the human's own rules.
const customCategory = "custom"

// Extend adds the human's own rules to the set.
//
// A command pattern is read like a command, with the same parser: the rule
// fires on a command with the same program, a subcommand starting with the
// pattern's, and all of the pattern's flags. "git push --force" matches
// git -C repo push --force origin, and sudo git push --force; it does not
// match git push -f, which says the same in other words: list both.
func (s *Set) Extend(c Custom) error {
	if s.Categories == nil {
		s.Categories = map[string]string{}
	}
	if _, ok := s.Categories[customCategory]; !ok {
		s.Categories[customCategory] = "Rules from your own settings"
	}
	add := func(list []string, kind string, sev Severity) error {
		for i, p := range list {
			p = strings.TrimSpace(p)
			k, err := patternCond(p)
			if err != nil {
				return fmt.Errorf("review.rules.%s %q: %w", kind, p, err)
			}
			verb := map[string]string{"deny": "deny", "ask": "ask you about"}[kind]
			s.Rules = append(s.Rules, &Rule{ID: fmt.Sprintf("custom.%s.%d", kind, i+1), Category: customCategory,
				Severity: sev, On: OnCommand, match: k, Reason: fmt.Sprintf("Your settings %s `%s`.", verb, p)})
		}
		return nil
	}
	if err := add(c.Deny, "deny", Harmful); err != nil {
		return err
	}
	if err := add(c.Ask, "ask", Suspicious); err != nil {
		return err
	}
	for i, p := range c.Protect {
		p = strings.TrimSpace(p)
		g := p
		if !absolutePattern(g) {
			g = "{workspace}/" + strings.TrimPrefix(strings.ReplaceAll(g, `\`, "/"), "./")
		}
		s.Rules = append(s.Rules, &Rule{ID: fmt.Sprintf("custom.protect.%d", i+1), Category: customCategory,
			Severity: Harmful, On: OnPath, Reason: fmt.Sprintf("Your settings protect `%s` from changes.", p),
			match: &cond{access: []string{"write", "delete", "link", "meta"}, glob: []string{g}}})
	}
	s.index()
	return nil
}

// absolutePattern reports a glob that names its own place: absolute, the
// home folder, a placeholder, or any depth.
func absolutePattern(g string) bool {
	return strings.HasPrefix(g, "/") || strings.HasPrefix(g, "~") || strings.HasPrefix(g, "{") ||
		strings.HasPrefix(g, "%") || strings.HasPrefix(g, "**/") || strings.HasPrefix(g, `\\`) ||
		len(g) >= 2 && g[1] == ':'
}

// patternCond reads a command pattern into the keys of a command rule.
func patternCond(p string) (*cond, error) {
	call := facts.Shell(facts.Bash, p, "", facts.Env{OS: "linux", Workspace: "/w"})
	if call.Meta.ParseError {
		return nil, fmt.Errorf("it does not parse as a command")
	}
	var c *facts.Command
	for _, cmd := range call.Commands {
		if cmd.Program != "" && cmd.Depth == 0 && len(cmd.WrappedBy) == 0 {
			c = cmd
			break
		}
	}
	if c == nil {
		return nil, fmt.Errorf("it names no program")
	}
	k := &cond{program: []string{c.Program}}
	if c.Subcommand != "" {
		k.subcommand = []string{c.Subcommand}
	}
	// -rf in a pattern means -r and -f, which a command may write apart.
	singles := map[string]bool{}
	for _, f := range c.Flags {
		if len(f.Name) == 2 {
			singles[f.Name] = true
		}
	}
	for _, f := range c.Flags {
		if group := len(f.Name) > 2 && f.Name[0] == '-' && f.Name[1] != '-'; group {
			all := true
			for _, ch := range f.Name[1:] {
				all = all && singles["-"+string(ch)]
			}
			if all {
				continue
			}
		}
		k.flagsAll = append(k.flagsAll, f.Name)
	}
	return k, nil
}
