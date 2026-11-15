package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"ritam/internal/config"
	"ritam/internal/fsutil"
	"ritam/internal/layout"
	"ritam/internal/ports"
)

// spendKeepDays is how many days of spending the ledger keeps.
const spendKeepDays = 31

// spendLockWait is how long an Add waits for another ritam to finish its
// own. Each holds the lock for one small read and write, so this is only
// reached when something is badly wrong (a stuck disk).
const spendLockWait = 10 * time.Second

// dayTotals is one day's spending in the ledger file.
type dayTotals struct {
	CostUSD float64 `json:"costUsd"`
	Tokens  int64   `json:"tokens"`
	// UTCOffset is the local offset when the day was last added to, so a
	// day that looks short or long (a clock change, a trip) can be
	// explained.
	UTCOffset string `json:"utcOffset,omitempty"`
}

// spendLedger keeps the day's spending in a file in the app home, shared by
// every project and every ritam running on this machine: each Add reads the
// file, adds, and writes it back under an OS file lock, so two instances add
// up rather than each counting its own or overwriting the other's. Agents
// cannot write the app home (it is outside every workspace, and denied to
// Claude's file tools by name).
//
// A day is the local calendar day: the count starts again at local
// midnight, which is when a human expects "today's spending" to reset.
type spendLedger struct {
	// mu orders this process's own adds, so they queue here rather than
	// spin on the file lock.
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
//
// An error means the day's spending is not known for sure; the caller must
// not take the totals as under the limit. A ledger that cannot be parsed is
// kept aside as spend.json.bad-<time> for a human to read, and the day is
// counted again from this turn, with an error saying so: quietly starting
// from $0 would let a day spend its limit twice.
func (l *spendLedger) Add(u ports.Usage) (float64, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := fsutil.Lock(l.path+".lock", spendLockWait)
	if err != nil {
		return 0, 0, fmt.Errorf("daily spend record: %w", err)
	}
	defer unlock()

	days, readErr := l.read(true)
	var lost *badLedgerError
	if readErr != nil && !errors.As(readErr, &lost) {
		return 0, 0, readErr
	}
	now := l.now().Local()
	today := now.Format("2006-01-02")
	d := days[today]
	d.CostUSD += u.CostUSD
	d.Tokens += u.InputTokens + u.OutputTokens
	d.UTCOffset = now.Format("-07:00")
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
	if err := fsutil.WriteAtomic(l.path, out); err != nil {
		return d.CostUSD, d.Tokens, fmt.Errorf("daily spend record: %w", err)
	}
	return d.CostUSD, d.Tokens, readErr
}

// Today returns today's totals without adding to them, for showing.
func (l *spendLedger) Today() (costUSD float64, tokens int64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Locked too: on Windows a reader holding the file open would make a
	// writer's rename over it fail.
	unlock, err := fsutil.Lock(l.path+".lock", spendLockWait)
	if err != nil {
		return 0, 0, fmt.Errorf("daily spend record: %w", err)
	}
	defer unlock()
	// A bad file is left where it is: the next Add keeps it aside and
	// reports it to the caller that decides what to do about it.
	days, err := l.read(false)
	if err != nil {
		return 0, 0, err
	}
	d := days[l.now().Local().Format("2006-01-02")]
	return d.CostUSD, d.Tokens, nil
}

// badLedgerError reports a ledger that could not be parsed and was kept
// aside.
type badLedgerError struct {
	path, keptAs string
	err          error
}

func (e *badLedgerError) Error() string {
	return fmt.Sprintf("daily spend record %s could not be read (%v): it was kept as %s, "+
		"and today's spending is counted again from now", e.path, e.err, e.keptAs)
}

func (e *badLedgerError) Unwrap() error { return e.err }

// read loads the ledger, empty when there is none yet. The caller holds the
// lock. A file that does not parse is an error; with setAside it is moved
// aside and reported with a *badLedgerError alongside an empty ledger.
func (l *spendLedger) read(setAside bool) (map[string]dayTotals, error) {
	days := map[string]dayTotals{}
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return days, nil
	}
	if err != nil {
		return nil, fmt.Errorf("daily spend record: %w", err)
	}
	if err := json.Unmarshal(data, &days); err != nil {
		if !setAside {
			return nil, fmt.Errorf("daily spend record %s could not be read: %w", l.path, err)
		}
		keptAs, mvErr := fsutil.SetAside(l.path)
		if mvErr != nil {
			return nil, fmt.Errorf("daily spend record %s could not be read (%v) nor kept aside: %w", l.path, err, mvErr)
		}
		return map[string]dayTotals{}, &badLedgerError{path: l.path, keptAs: keptAs, err: err}
	}
	if days == nil { // the file held "null"
		days = map[string]dayTotals{}
	}
	return days, nil
}
