// Package config loads the local configuration file. Configuration is local
// only: there is no remote config source, and secrets never live here.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"ritam/internal/domain"
)

// DefaultPath is the configuration file looked up relative to the working
// directory when no explicit path is given.
const DefaultPath = "ritam.yaml"

// AgentConfig configures one registered agent.
//
// Executable paths are configurable and never hard-coded: Claude Code and
// Codex may be installed anywhere, and on Windows they are frequently shims on
// PATH rather than real binaries at a known location.
type AgentConfig struct {
	// Type selects the adapter: claude-code, codex or mock.
	Type string `yaml:"type"`
	// DisplayName is what the TUI shows. Defaults to the agent key.
	DisplayName string `yaml:"displayName"`
	// Role is AUTHORITY, IMPLEMENTER, REVIEWER or OBSERVER.
	Role    string `yaml:"role"`
	Enabled bool   `yaml:"enabled"`
	// Executable is the command to launch, resolved on PATH when not absolute.
	Executable string `yaml:"executable"`
	// Args are extra arguments passed to the executable.
	Args []string `yaml:"args"`
	// WorkingDir is the directory the agent process runs in, relative to the
	// workspace. Empty means the workspace root; it may not lie outside it.
	WorkingDir string `yaml:"workingDir"`
	// StartupTimeout bounds how long the runtime may take to become ready.
	StartupTimeout time.Duration `yaml:"startupTimeout"`
	// ResponseTimeout bounds how long a single request to the agent may take.
	ResponseTimeout time.Duration `yaml:"responseTimeout"`
	// Model optionally overrides the model the CLI would pick itself.
	Model string `yaml:"model"`
	// AllowWrites lets the agent edit files and run commands. The field is
	// phrased positively so the zero value is the safe one: a config that
	// omits it gets a read-only agent, never a writing one.
	AllowWrites bool `yaml:"allowWrites"`
	// AllowedCommands are shell commands this agent may run when it may
	// write, as command prefixes ("go test"), on top of the workspace's.
	// Destructive commands are refused whatever is listed here.
	AllowedCommands []string `yaml:"allowedCommands"`
	// ScratchDir is where ritam keeps an agent's short-lived files (Codex's
	// reply file). It is set from the workspace, never configured: ritam
	// writes nothing outside the workspace, temp folders included.
	ScratchDir string `yaml:"-"`
}

// OrchestratorConfig holds coordination and loop-protection settings.
type OrchestratorConfig struct {
	MaxConcurrentTasks int `yaml:"maxConcurrentTasks"`
	// The limits below meter autonomous agent work only: any human input
	// (instruction, follow-up, resume, approval) starts a fresh allowance, and
	// messages to or from the human never count. See domain.TaskLimits.
	//
	// MaxDeliberationRounds is the checkpoint interval: every this many
	// authority-led agent rounds without human input, the authority posts a
	// progress update and the human is notified. Work continues.
	MaxDeliberationRounds int `yaml:"maxDeliberationRounds"`
	// MaxMessages pauses after this many agent-to-agent messages without
	// human input.
	MaxMessages int `yaml:"maxMessages"`
	// MaxTaskDuration pauses after this long without human input.
	MaxTaskDuration time.Duration `yaml:"maxTaskDuration"`
	// StallMessages pauses after this many consecutive agent messages that
	// make no progress (no request answered, nothing new, or a repeat).
	StallMessages int `yaml:"stallMessages"`
	// MaxTotalMessages is the absolute ceiling over a task's life; nothing
	// resets it.
	MaxTotalMessages int `yaml:"maxTotalMessages"`
	// MaxTaskCostUSD and MaxTaskTokens pause a task whose agents have spent
	// this much, as the CLIs report it. Zero disables each. Codex reports
	// tokens but not cost, so only MaxTaskTokens bounds Codex spending.
	MaxTaskCostUSD float64 `yaml:"maxTaskCostUSD"`
	MaxTaskTokens  int64   `yaml:"maxTaskTokens"`
	// DefaultAuthority is the agent that holds authority when the human does
	// not choose one.
	DefaultAuthority string `yaml:"defaultAuthority"`
	// RequireHumanApproval holds a task at AWAITING_DECISION until the human
	// approves or rejects the authority agent's FINAL_DECISION.
	RequireHumanApproval bool `yaml:"requireHumanApproval"`
	// IdleTimeout pauses a task when no agent speaks for this long, so a hung
	// agent cannot leave a task silently stuck.
	IdleTimeout time.Duration `yaml:"idleTimeout"`
}

// KnowledgeConfig is kept so existing config files still load. Shared
// knowledge is stored with everything else, in the backend Storage selects.
type KnowledgeConfig struct {
	Backend string `yaml:"backend"`
	Path    string `yaml:"path"`
}

// StorageConfig selects where tasks, messages, events, decisions and
// knowledge are kept.
type StorageConfig struct {
	// Backend is "sqlite" (persistent, the default) or "memory" (everything
	// is lost when ritam exits).
	Backend string `yaml:"backend"`
	// Path is the SQLite database file. Empty, the default, means the
	// project's own database in its hidden folder.
	Path string `yaml:"path"`
}

// BackupConfig says how each project's database is backed up into the app
// home.
type BackupConfig struct {
	// Enabled turns backups on (the default).
	Enabled bool `yaml:"enabled"`
	// Keep is how many snapshots are kept per project.
	Keep int `yaml:"keep"`
	// Interval is how often a snapshot is taken while work is going on.
	// Nothing is written while the project is idle.
	Interval time.Duration `yaml:"interval"`
	// WhenProjectDeleted is what happens to a deleted project's backups:
	// "ask" (the default: say so, and keep them until told), "keep", or
	// "delete" (after DeleteAfterDays).
	WhenProjectDeleted string `yaml:"whenProjectDeleted"`
	// DeleteAfterDays is how long a deleted project's backups are kept
	// under "delete".
	DeleteAfterDays int `yaml:"deleteAfterDays"`
}

// Runtime is where this run keeps its files, worked out at start from the
// app home and the project (see package layout). It is never read from or
// written to a settings file.
type Runtime struct {
	// HomeDir is the app home; empty in tests and wherever no home is used.
	HomeDir string
	// ModelsFile records what was learned about models on this account.
	ModelsFile string
	// OwnWorkspace is where agents work after a global launch.
	OwnWorkspace string
	// ProjectID and ProjectName identify the project.
	ProjectID, ProjectName string
	// BackupDir is where the project's snapshots go.
	BackupDir string
	// ScratchDir is the project's tmp folder for the agents.
	ScratchDir string
	// TeamFile records the project's recruited agents.
	TeamFile string
	// ProjectConfig is the project's settings file, where the settings tab
	// saves the access level.
	ProjectConfig string
	// CanaryDir is where sandbox checks try to write, outside the workspace.
	CanaryDir string
	// GlobalAccess is the access the global settings file alone sets: the
	// user's own file on this machine, trusted as it is.
	GlobalAccess AccessConfig
	// Grant is the access approved on this machine for this project in this
	// folder, nil when none was. SaveGrant records a new approval.
	Grant     *AccessConfig
	SaveGrant func(AccessConfig) error
}

// ProxyConfig configures the future local LLM proxy. The proxy is not the
// orchestrator: it only handles provider traffic.
type ProxyConfig struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	// LogRequests enables request logging in the proxy once implemented.
	LogRequests bool `yaml:"logRequests"`
}

// LoggingConfig configures the structured logger.
type LoggingConfig struct {
	// Level is debug, info, warn or error.
	Level string `yaml:"level"`
	// Format is text or json.
	Format string `yaml:"format"`
	// File, when set, writes logs to that path instead of stderr. The TUI owns
	// the terminal, so logging to a file is the normal mode.
	File string `yaml:"file"`
}

// TUIConfig configures the terminal UI.
type TUIConfig struct {
	// DemoOnStart creates the mock demo task as soon as the TUI is ready.
	DemoOnStart bool `yaml:"demoOnStart"`
	// UseMockAgents forces every agent to use the mock adapter regardless of
	// its configured type. The scaffold defaults to true so that nothing ever
	// reaches a real provider by accident.
	UseMockAgents bool `yaml:"useMockAgents"`
}

// ProviderConfig says how to run one provider's CLI. It defines no agent:
// agents are recruited from the new-task form, one per model picked, and
// every agent of a provider is started the way its entry here says.
type ProviderConfig struct {
	// Executable is the CLI command: "claude" or "codex" when unset.
	Executable      string        `yaml:"executable"`
	StartupTimeout  time.Duration `yaml:"startupTimeout"`
	ResponseTimeout time.Duration `yaml:"responseTimeout"`
	// AllowWrites lets this provider's agents edit files, only ever inside
	// the workspace. Unset means yes; false makes them read-only.
	AllowWrites *bool `yaml:"allowWrites"`
	// AllowedCommands are commands this provider's agents may run besides
	// the workspace's own list.
	AllowedCommands []string `yaml:"allowedCommands"`
}

// MayWrite reports whether the provider's agents may edit files.
func (p ProviderConfig) MayWrite() bool { return p.AllowWrites == nil || *p.AllowWrites }

// Config is the whole local configuration.
type Config struct {
	// Providers says how to run each CLI, keyed claude-code and codex.
	Providers map[string]ProviderConfig `yaml:"providers"`
	// Agents is the older way of fixing the team in this file. Leave it out:
	// the team is recruited from the new-task form and kept in
	// .ritam/team.json. Agents listed here still load.
	Agents       map[string]AgentConfig `yaml:"agents"`
	Orchestrator OrchestratorConfig     `yaml:"orchestrator"`
	Knowledge    KnowledgeConfig        `yaml:"knowledge"`
	Proxy        ProxyConfig            `yaml:"proxy"`
	Logging      LoggingConfig          `yaml:"logging"`
	TUI          TUIConfig              `yaml:"tui"`
	Sessions     SessionsConfig         `yaml:"sessions"`
	Storage      StorageConfig          `yaml:"storage"`
	Workspace    WorkspaceConfig        `yaml:"workspace"`
	Access       AccessConfig           `yaml:"access"`
	Backups      BackupConfig           `yaml:"backups"`

	// Runtime is filled in at start, not configured.
	Runtime Runtime `yaml:"-"`
}

// SessionsConfig locates the record of which CLI session belongs to which
// agent on which task. Agents resume their own Claude / Codex session on
// every turn, so this file is what lets a follow-up -- even after a restart --
// continue the same conversation instead of starting a new one.
type SessionsConfig struct {
	// File is the registry path. The app sets it to the project's own file
	// at start; empty (as in tests) keeps the registry in memory, so
	// sessions then resume within one run only.
	File string `yaml:"file"`
}

// Default returns the configuration used when no file is present. It defines
// no agents: the team is recruited from the new-task form, one agent per
// model picked, and kept in .ritam/team.json.
func Default() *Config {
	return &Config{
		Providers: map[string]ProviderConfig{
			"claude-code": {Executable: "claude", StartupTimeout: 30 * time.Second, ResponseTimeout: 5 * time.Minute},
			"codex":       {Executable: "codex", StartupTimeout: 30 * time.Second, ResponseTimeout: 5 * time.Minute},
		},
		Orchestrator: OrchestratorConfig{
			MaxConcurrentTasks:    1,
			MaxDeliberationRounds: 10,
			MaxMessages:           200,
			MaxTaskDuration:       30 * time.Minute,
			StallMessages:         8,
			MaxTotalMessages:      1000,
			MaxTaskCostUSD:        10,
			IdleTimeout:           60 * time.Second,
		},
		Knowledge: KnowledgeConfig{Backend: "memory"},
		Proxy:     ProxyConfig{Enabled: false, Host: "127.0.0.1", Port: 8787},
		Logging:   LoggingConfig{Level: "info", Format: "text"},
		Storage:   StorageConfig{Backend: "sqlite"},
		TUI:       TUIConfig{DemoOnStart: false, UseMockAgents: true},
		Backups: BackupConfig{Enabled: true, Keep: 5, Interval: 10 * time.Minute,
			WhenProjectDeleted: "ask", DeleteAfterDays: 30},
	}
}

// Load reads configuration from path, falling back to defaults when the file
// does not exist. Values present in the file override the defaults.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}
	return LoadLayers(path)
}

// LoadLayers reads the settings files in order, each overriding the ones
// before it: the defaults, then the global settings in the app home, then
// the project's. A file that does not exist is skipped. Only what a file
// sets is overridden; a provider entry overrides field by field, so a
// project can change one setting of a provider and keep the rest.
func LoadLayers(paths ...string) (*Config, error) {
	cfg := Default()
	for _, path := range paths {
		if err := cfg.overlay(path); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// overlay applies one settings file on top of cfg.
func (c *Config) overlay(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config %s: %w", path, err)
	}
	before := map[string]ProviderConfig{}
	for k, v := range c.Providers {
		before[k] = v
	}
	// Decoding into the existing values keeps whatever the file leaves out.
	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	// A map entry is decoded whole, so a provider the file mentions would
	// lose the fields it leaves out; merge those back.
	var layer struct {
		Providers map[string]ProviderConfig `yaml:"providers"`
	}
	if err := yaml.Unmarshal(data, &layer); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	for name, set := range layer.Providers {
		merged, ok := before[name]
		if !ok {
			continue
		}
		if set.Executable != "" {
			merged.Executable = set.Executable
		}
		if set.StartupTimeout != 0 {
			merged.StartupTimeout = set.StartupTimeout
		}
		if set.ResponseTimeout != 0 {
			merged.ResponseTimeout = set.ResponseTimeout
		}
		if set.AllowWrites != nil {
			merged.AllowWrites = set.AllowWrites
		}
		if set.AllowedCommands != nil {
			merged.AllowedCommands = set.AllowedCommands
		}
		c.Providers[name] = merged
	}
	return nil
}

// Validate checks internal consistency. It intentionally does not check that
// agent executables exist: the scaffold must start without Claude or Codex
// installed.
func (c *Config) Validate() error {
	switch c.Storage.Backend {
	case "sqlite", "memory": // an empty sqlite path means the project's database
	default:
		return fmt.Errorf("storage.backend must be sqlite or memory, got %q", c.Storage.Backend)
	}
	if !domain.AccessLevel(c.Access.Level).Valid() {
		return fmt.Errorf("access.level must be 0 to 3, got %d", c.Access.Level)
	}
	switch c.Backups.WhenProjectDeleted {
	case "", "ask", "keep", "delete":
	default:
		return fmt.Errorf("backups.whenProjectDeleted must be ask, keep or delete, got %q", c.Backups.WhenProjectDeleted)
	}
	for name, a := range c.Agents {
		switch a.Type {
		case "claude-code", "codex", "mock":
		default:
			return fmt.Errorf("config: agent %q has unknown type %q", name, a.Type)
		}
		switch strings.ToUpper(a.Role) {
		case "AUTHORITY", "IMPLEMENTER", "REVIEWER", "OBSERVER":
		default:
			return fmt.Errorf("config: agent %q has unknown role %q", name, a.Role)
		}
	}
	for name := range c.Providers {
		switch name {
		case "claude-code", "codex":
		default:
			return fmt.Errorf("config: unknown provider %q (claude-code or codex)", name)
		}
	}
	// The authority is ticked in the new-task form; a default naming an
	// agent this file does not define may be a recruit, so only a default
	// that contradicts the agents listed here is an error.
	if c.Orchestrator.DefaultAuthority != "" && len(c.Agents) > 0 {
		if _, ok := c.Agents[c.Orchestrator.DefaultAuthority]; !ok {
			return fmt.Errorf("config: defaultAuthority %q is not a configured agent",
				c.Orchestrator.DefaultAuthority)
		}
	}
	if c.Orchestrator.MaxDeliberationRounds <= 0 {
		return fmt.Errorf("config: maxDeliberationRounds must be positive")
	}
	if c.Proxy.Enabled && (c.Proxy.Port <= 0 || c.Proxy.Port > 65535) {
		return fmt.Errorf("config: proxy port %d out of range", c.Proxy.Port)
	}
	return nil
}

// ResolvePath makes a configured relative path absolute against the directory
// holding the config file. It keeps Windows and POSIX paths working the same.
func ResolvePath(configPath, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(configPath), p)
}
