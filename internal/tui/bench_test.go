package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func BenchmarkView(b *testing.B) {
	m := busyModel()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// BenchmarkViewColour is BenchmarkView under a colour profile. A test process
// has no TTY, so lipgloss renders plain text and the benchmark above never pays
// for style rendering, which real terminals do on every cell.
func BenchmarkViewColour(b *testing.B) {
	saved := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(saved)

	m := busyModel()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}
