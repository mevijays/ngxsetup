package tui

import (
	"time"

	"ngxsetup/internal/stats"
)

// tickMsg drives the refresh loop; each one schedules the next tick and
// kicks off a fresh sample.
type tickMsg time.Time

// statsMsg carries a completed sample. Fetching happens inside a tea.Cmd —
// real I/O, run by the bubbletea runtime off the Update goroutine — so
// Update itself stays a pure state transition, which is what makes it
// testable without a terminal or a live server.
type statsMsg struct {
	rows []stats.SiteStats
	host hostSummary
	err  error
}

// purgeStartedMsg / purgeDoneMsg bracket a cache-purge action triggered from
// the dashboard, so the UI can show "purging…" instead of looking frozen
// during the nginx reload a purge performs.
type purgeStartedMsg struct{ slug string }
type purgeDoneMsg struct {
	slug string
	err  error
}
