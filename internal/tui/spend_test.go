package tui

import (
	"context"
	"strings"
	"testing"

	"ritam/internal/ports"
)

// TestTheHeaderShowsTodaysSpendAndHidesItWithoutADailyLimit: the day's
// spending over every task was invisible until it paused one. With a daily
// limit set the header shows it, as a warning from 80% of the limit; with
// none there is nothing to show.
func TestTheHeaderShowsTodaysSpendAndHidesItWithoutADailyLimit(t *testing.T) {
	svc := newFakeService()
	m := swarmModel()
	m.width, m.height = 200, 40
	load := func(s ports.DailySpend) {
		t.Helper()
		svc.spend = s
		next, _ := m.Update(loadSpendCmd(context.Background(), svc)())
		m = next.(Model)
	}

	load(ports.DailySpend{CostUSD: 3.20})
	if out := stripANSI(m.View()); strings.Contains(out, "today") {
		t.Errorf("with no daily limit the header shows no day's spending:\n%s", strings.Split(out, "\n")[1])
	}

	load(ports.DailySpend{CostUSD: 3.20, MaxCostUSD: 50})
	if out := stripANSI(m.View()); !strings.Contains(out, "today $3.20 of $50") {
		t.Errorf("the header must show today's spending against the limit:\n%s", strings.Split(out, "\n")[1])
	}
	if chip := m.spendChip(); chip[0].style.GetForeground() != dimStyle.GetForeground() {
		t.Error("well under the limit the day's spending is not a warning")
	}

	load(ports.DailySpend{CostUSD: 41, MaxCostUSD: 50})
	if chip := m.spendChip(); chip[0].style.GetForeground() != warnStyle.GetForeground() {
		t.Error("from 80% of the limit the day's spending is shown as a warning")
	}
}
