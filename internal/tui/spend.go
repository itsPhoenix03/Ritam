package tui

import (
	"context"
	"fmt"
	"math"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/ports"
)

// Today's spending, in the header beside the task's own.
//
// The daily limit over every task was invisible until it paused one: the
// human could not see a day running out, nor that the task about to start
// had a few cents left to work with.

// spendLoadedMsg carries today's spending.
type spendLoadedMsg struct {
	spend ports.DailySpend
	err   error
}

// loadSpendCmd reads today's spending. It is a file read, so it runs at
// start and after each turn is billed, never per frame.
func loadSpendCmd(ctx context.Context, svc ports.Service) tea.Cmd {
	return func() tea.Msg {
		s, err := svc.SpendToday(ctx)
		return spendLoadedMsg{spend: s, err: err}
	}
}

// spendWarnAt is the share of the daily limit from which it is shown as a
// warning.
const spendWarnAt = 0.8

// spendChip is the header's "today $3.20 of $50", nothing with no daily
// limit set. A record that cannot be read is shown as such: the tasks pause
// on it, and the human should see why before they do.
func (m Model) spendChip() line {
	s := m.spendToday
	if s.MaxCostUSD <= 0 && s.MaxTokens <= 0 {
		return nil
	}
	if m.spendErr != nil {
		return line{st("today: spend record unreadable", errorStyle)}
	}
	text, used := "", 0.0
	if s.MaxCostUSD > 0 {
		text, used = fmt.Sprintf("today $%.2f of $%s", s.CostUSD, dollars(s.MaxCostUSD)), s.CostUSD/s.MaxCostUSD
	} else {
		text, used = fmt.Sprintf("today %s of %s tok", compactCount(s.Tokens), compactCount(s.MaxTokens)),
			float64(s.Tokens)/float64(s.MaxTokens)
	}
	style := dimStyle
	if used >= spendWarnAt {
		style = warnStyle
	}
	return line{st(text, style)}
}

// dollars writes a limit without cents when it has none: "$50", "$12.50".
func dollars(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}
