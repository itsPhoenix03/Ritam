package config

import (
	"fmt"
	"time"
)

// ReviewConfig says how the agents' tool calls are approved. Every call goes
// through ritam's review (internal/review) before it runs, whatever the mode;
// the mode decides who approves what the rules let through.
type ReviewConfig struct {
	// Mode is auto or manual. Auto: ritam's rules decide; a harmful call is
	// refused, a suspicious one is asked about, the rest run. Manual: on top
	// of that, every command waits for the human's approval. Rules can miss
	// a destructive command, so auto is not a guarantee.
	Mode string `yaml:"mode"`
	// AskTimeout is how long a call waits for the human's answer before it
	// is refused.
	AskTimeout time.Duration `yaml:"askTimeout"`
}

const (
	// ReviewAuto lets ritam's rules decide.
	ReviewAuto = "auto"
	// ReviewManual asks the human before every command.
	ReviewManual = "manual"
	// DefaultAskTimeout is how long a call waits for the human by default.
	DefaultAskTimeout = 5 * time.Minute
	// MaxAskTimeout bounds the wait: the agent's CLI waits for the hook for
	// as long, and the turn's own clock keeps running meanwhile.
	MaxAskTimeout = 30 * time.Minute
	minAskTimeout = 10 * time.Second
)

// AutoModeWarning is what the human is told about auto mode wherever it can
// be chosen.
const AutoModeWarning = "auto: ritam's rules decide, asking you only about suspicious calls. " +
	"Rules cannot foresee everything, so a destructive command can still get through; " +
	"choose manual to approve every command yourself."

// Manual reports whether every command waits for the human.
func (r ReviewConfig) Manual() bool { return r.Mode == ReviewManual }

// Wait is how long a call waits for the human, the default when unset.
func (r ReviewConfig) Wait() time.Duration {
	if r.AskTimeout <= 0 {
		return DefaultAskTimeout
	}
	return r.AskTimeout
}

func (r ReviewConfig) validate() []error {
	var errs []error
	if r.Mode != ReviewAuto && r.Mode != ReviewManual {
		errs = append(errs, fmt.Errorf("config: review.mode must be %s or %s, got %q", ReviewAuto, ReviewManual, r.Mode))
	}
	if r.AskTimeout != 0 && (r.AskTimeout < minAskTimeout || r.AskTimeout > MaxAskTimeout) {
		errs = append(errs, fmt.Errorf("config: review.askTimeout must be between %s and %s, got %s",
			minAskTimeout, MaxAskTimeout, r.AskTimeout))
	}
	return errs
}

// SaveReview writes the review settings into the settings file at path,
// creating it if needed. Everything else in the file is kept as it was.
func SaveReview(path string, r ReviewConfig) error {
	return saveSection(path, "review", r, "How the agents' tool calls are approved: auto (ritam's rules decide and "+
		"ask you about suspicious calls) or manual (you approve every command). Set from the settings tab.",
		"save review")
}
