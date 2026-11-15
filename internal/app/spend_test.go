package app

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ritam/internal/ports"
)

// TestTheDaysSpendingAddsUpAcrossRunsAndDays: what each run adds is kept in
// the app home, so a second ritam (or the next start) adds to the same day,
// and a new day starts from nothing.
func TestTheDaysSpendingAddsUpAcrossRunsAndDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spend.json")
	day := time.Date(2026, 11, 1, 10, 0, 0, 0, time.Local)
	clock := func() time.Time { return day }
	one, two := &spendLedger{path: path, now: clock}, &spendLedger{path: path, now: clock}

	_, _, _ = one.Add(ports.Usage{CostUSD: 1.25, InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5000})
	cost, tokens, err := two.Add(ports.Usage{CostUSD: 0.50, InputTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if cost != 1.75 || tokens != 130 {
		t.Errorf("both runs' spending must add up, cache reads not counted as tokens: $%.2f %d", cost, tokens)
	}

	day = day.AddDate(0, 0, 1)
	if cost, _, _ := one.Add(ports.Usage{CostUSD: 0.10}); cost != 0.10 {
		t.Errorf("a new day starts afresh: $%.2f", cost)
	}
	day = day.AddDate(0, 0, spendKeepDays+1)
	_, _, _ = one.Add(ports.Usage{})
	data, _ := os.ReadFile(path)
	if len(data) == 0 {
		t.Fatal("the ledger must be written")
	}
	if strings.Contains(string(data), "2026-11-01") {
		t.Errorf("days older than %d are dropped: %s", spendKeepDays, data)
	}
}

// TestTwoInstancesAddingAtOnceLoseNothing: two ritam instances share the
// ledger file. Each add is read, add, write under a file lock, so 400 adds
// of $1 from both at once come to exactly $400, and none fails.
func TestTwoInstancesAddingAtOnceLoseNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spend.json")
	day := time.Date(2026, 11, 1, 10, 0, 0, 0, time.Local)
	clock := func() time.Time { return day }
	ledgers := []*spendLedger{{path: path, now: clock}, {path: path, now: clock}}

	const each = 200
	var wg sync.WaitGroup
	errs := make(chan error, 2*each)
	for _, l := range ledgers {
		for i := 0; i < each; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := l.Add(ports.Usage{CostUSD: 1, InputTokens: 1}); err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("no add may fail: %v", err)
	}
	cost, tokens, err := ledgers[0].Today()
	if err != nil {
		t.Fatal(err)
	}
	if cost != 2*each || tokens != 2*each {
		t.Errorf("every add must count: $%.0f %d tokens, want $%d", cost, tokens, 2*each)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("no temporary file is left behind: %s", e.Name())
		}
	}
}

// TestACorruptLedgerIsKeptAsideAndReported: an unreadable ledger is not
// quietly taken as a day with nothing spent. It is kept for a human to read,
// Add says so, and the day is counted again from that turn.
func TestACorruptLedgerIsKeptAsideAndReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spend.json")
	if err := os.WriteFile(path, []byte(`{"2026-11-01": {"costUsd": 4`), 0o600); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 1, 10, 0, 0, 0, time.Local)
	l := &spendLedger{path: path, now: func() time.Time { return day }}

	if _, _, err := l.Today(); err == nil {
		t.Error("reading a corrupt ledger must be an error")
	}
	cost, _, err := l.Add(ports.Usage{CostUSD: 0.5})
	if err == nil || !strings.Contains(err.Error(), "spend.json.bad-") {
		t.Fatalf("Add must report the corrupt ledger and where it was kept: %v", err)
	}
	if cost != 0.5 {
		t.Errorf("the day is counted again from this turn: $%.2f", cost)
	}
	kept, _ := filepath.Glob(path + ".bad-*")
	if len(kept) != 1 {
		t.Fatalf("the corrupt file must be kept aside: %v", kept)
	}
	if data, _ := os.ReadFile(kept[0]); !strings.Contains(string(data), `"costUsd": 4`) {
		t.Errorf("the kept file is the corrupt one, unchanged: %s", data)
	}
	if _, _, err := l.Add(ports.Usage{CostUSD: 0.5}); err != nil {
		t.Errorf("once reported, the ledger counts on: %v", err)
	}
}

// TestTheDayIsTheLocalCalendarDay: spending just before and just after
// local midnight falls on two days.
func TestTheDayIsTheLocalCalendarDay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spend.json")
	day := time.Date(2026, 11, 1, 23, 59, 0, 0, time.Local)
	l := &spendLedger{path: path, now: func() time.Time { return day }}
	_, _, _ = l.Add(ports.Usage{CostUSD: 3})
	day = day.Add(2 * time.Minute)
	if cost, _, _ := l.Add(ports.Usage{CostUSD: 1}); cost != 1 {
		t.Errorf("a new local day starts at local midnight: $%.2f", cost)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "2026-11-01") || !strings.Contains(string(data), "2026-11-02") {
		t.Errorf("both local days are recorded: %s", data)
	}
}
