package app

import (
	"os"
	"path/filepath"
	"strings"
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
