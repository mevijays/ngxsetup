package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ngxsetup/internal/stats"
)

// Run starts the dashboard and blocks until the operator quits (q or
// ctrl+c). sampler is expected to already be wired to a database client via
// stats.NewSampler — this package has no opinion on where that comes from,
// which is what keeps it independent of provision/db and testable on its
// own. interval <= 0 uses DefaultInterval.
func Run(sites SiteProvider, sampler *stats.Sampler, purger CachePurger, interval time.Duration) error {
	m := New(sites, sampler, purger, interval)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
