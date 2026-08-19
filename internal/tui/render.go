package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"ngxsetup/internal/stats"
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
)

// rowsToTable formats sampled stats into table rows. Formatting lives here,
// separate from Update, so a change to how a number is displayed can never
// accidentally change sorting or keybinding behavior — the two are read from
// the same []stats.SiteStats but cannot leak into each other.
func rowsToTable(rows []stats.SiteStats, purging map[string]bool) []table.Row {
	out := make([]table.Row, 0, len(rows))
	for _, r := range rows {
		state := "ok"
		switch {
		case purging[r.Slug]:
			state = "purging…"
		case r.Err != nil:
			state = "no data"
		case r.Workers == 0:
			state = "idle"
		}
		out = append(out, table.Row{
			r.Domain,
			formatCPU(r.CPUPercent, r.Err),
			formatMem(r.MemoryMB, r.Err),
			fmt.Sprintf("%d/%d", r.Workers, r.MaxWorkers),
			formatReqRate(r.ReqPerSec),
			formatHitPercent(r.CacheHitPercent),
			formatDBSize(r.DBSizeMB),
			state,
		})
	}
	return out
}

func formatCPU(pct float64, err error) string {
	if err != nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", pct)
}

func formatMem(mb int, err error) string {
	if err != nil {
		return "—"
	}
	if mb >= 1024 {
		return fmt.Sprintf("%.1fG", float64(mb)/1024)
	}
	return fmt.Sprintf("%dM", mb)
}

func formatReqRate(perSec float64) string {
	return fmt.Sprintf("%.2f", perSec)
}

func formatHitPercent(pct float64) string {
	if pct < 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", pct)
}

func formatDBSize(mb int64) string {
	if mb <= 0 {
		return "—"
	}
	if mb >= 1024 {
		return fmt.Sprintf("%.1fG", float64(mb)/1024)
	}
	return fmt.Sprintf("%dM", mb)
}

// View renders the whole screen: a host summary line, the table, and a
// footer with keybindings and the last status message.
func (m Model) View() string {
	if m.quitting {
		return ""
	}

	var b []byte
	b = append(b, headerStyle.Render("ngxsetup top")...)
	b = append(b, " — live per-site resource usage\n\n"...)
	b = append(b, m.hostLine()...)
	b = append(b, "\n\n"...)
	b = append(b, m.table.View()...)
	b = append(b, "\n\n"...)
	b = append(b, m.footer()...)
	return string(b)
}

func (m Model) hostLine() string {
	h := m.host
	loadStyle := okStyle
	if h.CPUCores > 0 && h.Load1 > float64(h.CPUCores) {
		loadStyle = warnStyle
	}
	memPct := 0
	if h.MemTotalMB > 0 {
		memPct = h.MemUsedMB * 100 / h.MemTotalMB
	}
	memStyle := okStyle
	if memPct >= 90 {
		memStyle = errStyle
	} else if memPct >= 75 {
		memStyle = warnStyle
	}
	return fmt.Sprintf(
		"host: %d cores  load %s  mem %s (%d%%)  sites: %d",
		h.CPUCores,
		loadStyle.Render(fmt.Sprintf("%.2f/%.2f", h.Load1, h.Load5)),
		memStyle.Render(fmt.Sprintf("%d/%d MB", h.MemUsedMB, h.MemTotalMB)),
		memPct,
		len(m.rows),
	)
}

func (m Model) footer() string {
	keys := dimStyle.Render(
		"↑/↓ move  c/m/r/d sort  p purge cache  q quit",
	)
	if m.err != nil {
		return keys + "\n" + errStyle.Render("last sample failed: "+m.err.Error())
	}
	if m.statusMsg != "" {
		return keys + "\n" + dimStyle.Render(m.statusMsg)
	}
	return keys
}
