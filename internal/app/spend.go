package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ritam/internal/config"
	"ritam/internal/layout"
	"ritam/internal/ports"
)

// spendKeepDays is how many days of spending the ledger keeps.
const spendKeepDays = 31

// dayTotals is one day's spending in the ledger file.
type dayTotals struct {
	CostUSD float64 `json:"costUsd"`
	Tokens  int64   `json:"tokens"`
}

// spendLedger keeps the day's spending in a file in the app home, shared by
// every project and every ritam running on this machine: each Add reads the
// file, adds, and writes it back, so two instances add up rather than each
// counting its own. Agents cannot write the app home (it is outside every
// workspace, and denied to Claude's file tools by name).
type spendLedger struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

// dailyLedger is the run's spending ledger, nil without an app home (tests)
// or with no daily limit set.
func dailyLedger(cfg *config.Config) ports.SpendLedger {
	o := cfg.Orchestrator
	if cfg.Runtime.HomeDir == "" || o.MaxDailyCostUSD <= 0 && o.MaxDailyTokens <= 0 {
		return nil
	}
	return &spendLedger{path: layout.Home{Root: cfg.Runtime.HomeDir}.SpendFile(), now: time.Now}
}

// Add records one turn's usage and returns today's totals with it. Cache
// reads count toward the cost (the CLI prices them in) but not the tokens,
// the same as a task's own budget.
func (l *spendLedger) Add(u ports.Usage) (float64, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	days := map[string]dayTotals{}
	data, err := os.ReadFile(l.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &days); err != nil {
			days = map[string]dayTotals{} // unreadable: start again rather than stop counting
		}
	case !errors.Is(err, os.ErrNotExist):
		return 0, 0, err
	}
	now := l.now()
	today := now.Format("2006-01-02")
	d := days[today]
	d.CostUSD += u.CostUSD
	d.Tokens += u.InputTokens + u.OutputTokens
	days[today] = d
	oldest := now.AddDate(0, 0, -spendKeepDays).Format("2006-01-02")
	for day := range days {
		if day < oldest {
			delete(days, day)
		}
	}
	out, err := json.MarshalIndent(days, "", "  ")
	if err != nil {
		return d.CostUSD, d.Tokens, err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return d.CostUSD, d.Tokens, err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return d.CostUSD, d.Tokens, err
	}
	return d.CostUSD, d.Tokens, os.Rename(tmp, l.path)
}
