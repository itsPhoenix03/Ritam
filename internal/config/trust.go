package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"time"
)

// A project's settings files travel with it: a clone or a copy carries its
// ritam.yaml and its hidden folder. So they are not trusted the way the
// global settings in the app home are, which only the user writes. Trusted
// handles the access level; RestrictProject the rest.

// RestrictProject undoes what the project's settings files (the layers on
// top of global) may not decide, and returns the settings held back, for
// the human to be told. root is the project folder.
//
// They may tighten anything. They may not:
//   - choose the programs ritam runs (a provider's or an agent's
//     executable) or add arguments to them;
//   - add commands the global settings do not list;
//   - point the workspace, the log, the database or the sessions outside
//     the project folder, turn read confinement off, or allow a global
//     workspace;
//   - loosen a limit: spending, messages, duration, timeouts, or the human's
//     sign-off on a final decision;
//   - touch the backups (they act on every project's, in the app home) or
//     the proxy.
//
// What is held back can be set in the global settings file instead, which
// is the user saying so on this machine.
func (c *Config) RestrictProject(global *Config, root string) (held []string) {
	hold := func(name string) { held = append(held, name) }

	trustedCmds := append([]string(nil), global.Access.Commands...)
	for _, p := range global.Providers {
		trustedCmds = append(trustedCmds, p.AllowedCommands...)
	}
	commands := func(name string, cmds []string, own []string) []string {
		var kept []string
		for _, cmd := range cmds {
			if slices.Contains(own, cmd) || slices.Contains(trustedCmds, cmd) {
				kept = append(kept, cmd)
			} else {
				hold(name + ": " + cmd)
			}
		}
		return kept
	}

	for name, p := range c.Providers {
		g := global.Providers[name]
		if executableOr(p.Executable, name) != executableOr(g.Executable, name) {
			hold("providers." + name + ".executable")
			p.Executable = g.Executable
		}
		if p.MayWrite() && !g.MayWrite() {
			hold("providers." + name + ".allowWrites")
			p.AllowWrites = g.AllowWrites
		}
		p.AllowedCommands = commands("providers."+name+".allowedCommands", p.AllowedCommands, g.AllowedCommands)
		if ceiling := orDuration(g.ResponseTimeout, defaultResponseTimeout); p.ResponseTimeout > ceiling {
			hold("providers." + name + ".responseTimeout")
			p.ResponseTimeout = ceiling
		}
		c.Providers[name] = p
	}

	for name, a := range c.Agents {
		g := global.Agents[name]
		if executableOr(a.Executable, a.Type) != executableOr(g.Executable, a.Type) {
			hold("agents." + name + ".executable")
			a.Executable = g.Executable
		}
		if !slices.Equal(a.Args, g.Args) {
			hold("agents." + name + ".args")
			a.Args = g.Args
		}
		a.AllowedCommands = commands("agents."+name+".allowedCommands", a.AllowedCommands, g.AllowedCommands)
		if ceiling := orDuration(g.ResponseTimeout, defaultResponseTimeout); a.ResponseTimeout > ceiling {
			hold("agents." + name + ".responseTimeout")
			a.ResponseTimeout = ceiling
		}
		c.Agents[name] = a
	}

	inside := func(name string, field *string, trusted string) {
		if *field == trusted || *field == "" {
			return
		}
		p := *field
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		if !Within(root, p) {
			hold(name + " (outside the project)")
			*field = trusted
		}
	}
	inside("workspace.root", &c.Workspace.Root, global.Workspace.Root)
	inside("logging.file", &c.Logging.File, global.Logging.File)
	inside("storage.path", &c.Storage.Path, global.Storage.Path)
	inside("sessions.file", &c.Sessions.File, global.Sessions.File)
	inside("knowledge.path", &c.Knowledge.Path, global.Knowledge.Path)
	if global.Workspace.ConfineReads && !c.Workspace.ConfineReads {
		hold("workspace.confineReads")
		c.Workspace.ConfineReads = true
	}
	if c.Workspace.AllowGlobal && !global.Workspace.AllowGlobal {
		hold("workspace.allowGlobal")
		c.Workspace.AllowGlobal = false
	}

	o, g := &c.Orchestrator, global.Orchestrator
	limit := func(name string, v *int, trusted int) {
		if t := tighterInt(*v, trusted); t != *v {
			hold("orchestrator." + name)
			*v = t
		}
	}
	limit("maxConcurrentTasks", &o.MaxConcurrentTasks, g.MaxConcurrentTasks)
	limit("maxDeliberationRounds", &o.MaxDeliberationRounds, g.MaxDeliberationRounds)
	limit("maxMessages", &o.MaxMessages, g.MaxMessages)
	limit("stallMessages", &o.StallMessages, g.StallMessages)
	limit("maxTotalMessages", &o.MaxTotalMessages, g.MaxTotalMessages)
	if t := tighterDuration(o.MaxTaskDuration, g.MaxTaskDuration); t != o.MaxTaskDuration {
		hold("orchestrator.maxTaskDuration")
		o.MaxTaskDuration = t
	}
	if t := tighterInt64(o.MaxTaskTokens, g.MaxTaskTokens); t != o.MaxTaskTokens {
		hold("orchestrator.maxTaskTokens")
		o.MaxTaskTokens = t
	}
	if t := tighterFloat(o.MaxTaskCostUSD, g.MaxTaskCostUSD); t != o.MaxTaskCostUSD {
		hold("orchestrator.maxTaskCostUSD")
		o.MaxTaskCostUSD = t
	}
	if t := tighterFloat(o.MaxDailyCostUSD, g.MaxDailyCostUSD); t != o.MaxDailyCostUSD {
		hold("orchestrator.maxDailyCostUSD")
		o.MaxDailyCostUSD = t
	}
	if t := tighterInt64(o.MaxDailyTokens, g.MaxDailyTokens); t != o.MaxDailyTokens {
		hold("orchestrator.maxDailyTokens")
		o.MaxDailyTokens = t
	}
	if g.RequireHumanApproval && !o.RequireHumanApproval {
		hold("orchestrator.requireHumanApproval")
		o.RequireHumanApproval = true
	}

	if c.Access.AllowUnsandboxed && !global.Access.AllowUnsandboxed {
		hold("access.allowUnsandboxed")
		c.Access.AllowUnsandboxed = false
	}
	if c.Backups != global.Backups {
		hold("backups")
		c.Backups = global.Backups
	}
	if c.Proxy != global.Proxy {
		hold("proxy")
		c.Proxy = global.Proxy
	}
	return held
}

// HeldNotice is what the human is told about settings held back.
func HeldNotice(held []string, globalPath string) string {
	if len(held) == 0 {
		return ""
	}
	return fmt.Sprintf("this project's settings files ask for things only your own settings may decide, "+
		"so these were ignored: %v. A cloned or copied project cannot choose the programs ritam runs, "+
		"their arguments, places outside the project, or looser limits; set them in %s if you mean them",
		held, globalPath)
}

// defaultResponseTimeout is what a CLI call may take when nothing says.
const defaultResponseTimeout = 5 * time.Minute

// executableOr is an executable, or the provider's usual CLI when unset.
func executableOr(exe, provider string) string {
	switch {
	case exe != "":
		return exe
	case provider == "claude-code":
		return "claude"
	}
	return provider
}

func orDuration(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// tighterInt and its kin pick the tighter of a project's limit and the
// trusted one, where zero (or less) means no limit.
func tighterInt(v, trusted int) int {
	if trusted > 0 && (v <= 0 || v > trusted) {
		return trusted
	}
	return v
}

func tighterInt64(v, trusted int64) int64 {
	if trusted > 0 && (v <= 0 || v > trusted) {
		return trusted
	}
	return v
}

func tighterFloat(v, trusted float64) float64 {
	if trusted > 0 && (v <= 0 || v > trusted) {
		return trusted
	}
	return v
}

func tighterDuration(v, trusted time.Duration) time.Duration {
	if trusted > 0 && (v <= 0 || v > trusted) {
		return trusted
	}
	return v
}
