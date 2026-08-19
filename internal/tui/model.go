// Package tui is the live per-site resource dashboard (`ngxsetup top`).
//
// Built on bubbletea's Elm architecture deliberately: Update is a pure state
// transition (msg in, new Model + next Cmd out) with all real I/O — sampling
// PHP-FPM, tailing logs, purging a cache — pushed into tea.Cmd functions the
// runtime executes off to the side. That split is what makes the keybinding
// and sorting logic in this file testable by feeding it fake messages, the
// same way the rest of this codebase keeps arithmetic and decisions provable
// without a live server wherever the underlying operation allows it.
package tui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ngxsetup/internal/stats"
)

// DefaultInterval is how often the dashboard resamples. Fast enough that CPU%
// feels live, slow enough that the dashboard itself is not a meaningful load
// on a small box — the whole reason this tool exists is to run well there.
const DefaultInterval = 2 * time.Second

// chromeHeight is how many lines View() spends on everything that is not the
// table itself: title, blank, host line, blank, blank-after-table, footer
// keys, footer status. minRows is the smallest table we are willing to
// render before it stops being useful. table.SetHeight subtracts its own
// header's rendered height (2 lines, with the bold+border-bottom style this
// package applies) from whatever is passed in, so the input has to be
// minRows+2 for Height() to actually report minRows afterward — confirmed
// against bubbles/table's own SetHeight source, not assumed.
const (
	chromeHeight  = 9
	minRows       = 3
	minTableInput = minRows + 2
)

// SiteProvider supplies the current site list on each tick, so sites added or
// removed in another terminal appear without restarting the dashboard.
type SiteProvider interface {
	Sites() []stats.Site
}

// CachePurger performs the one mutating action reachable from the dashboard.
// Kept to this single, safe, idempotent operation deliberately — a dashboard
// is not the place to fat-finger something as consequential as removing a
// site; that stays a deliberate CLI command with its own confirmation.
type CachePurger interface {
	PurgeCache(domain string) error
}

type sortKey int

const (
	sortByCPU sortKey = iota
	sortByMem
	sortByReqs
	sortByDomain
)

func (k sortKey) label() string {
	switch k {
	case sortByMem:
		return "memory"
	case sortByReqs:
		return "req/s"
	case sortByDomain:
		return "domain"
	default:
		return "cpu"
	}
}

// Model is the dashboard's whole state.
type Model struct {
	sites    SiteProvider
	sampler  *stats.Sampler
	purger   CachePurger
	interval time.Duration

	table table.Model
	rows  []stats.SiteStats // parallel to table rows, same order, same index
	host  hostSummary

	sortBy   sortKey
	sortDesc bool

	err       error
	purging   map[string]bool
	statusMsg string

	width, height int
	quitting      bool
}

// New builds a dashboard model. interval <= 0 uses DefaultInterval.
func New(sites SiteProvider, sampler *stats.Sampler, purger CachePurger, interval time.Duration) Model {
	if interval <= 0 {
		interval = DefaultInterval
	}
	cols := columnsFor(sortByCPU, true)
	t := table.New(
		table.WithColumns(cols),
		table.WithFocused(true),
		table.WithHeight(15),
	)
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Bold(true).Foreground(lipgloss.Color("14")).BorderBottom(true)
	styles.Selected = styles.Selected.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("14")).Bold(true)
	t.SetStyles(styles)

	return Model{
		sites:    sites,
		sampler:  sampler,
		purger:   purger,
		interval: interval,
		table:    t,
		sortBy:   sortByCPU,
		sortDesc: true,
		purging:  make(map[string]bool),
	}
}

func columnsFor(sortBy sortKey, desc bool) []table.Column {
	arrow := func(k sortKey) string {
		if k != sortBy {
			return ""
		}
		if desc {
			return " ↓"
		}
		return " ↑"
	}
	return []table.Column{
		{Title: "DOMAIN" + arrow(sortByDomain), Width: 28},
		{Title: "CPU%" + arrow(sortByCPU), Width: 8},
		{Title: "MEM" + arrow(sortByMem), Width: 9},
		{Title: "WORKERS", Width: 9},
		{Title: "REQ/S" + arrow(sortByReqs), Width: 8},
		{Title: "CACHE%", Width: 8},
		{Title: "DB", Width: 9},
		{Title: "STATE", Width: 10},
	}
}

// Init kicks off the first sample and the tick loop.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetchCmd(), tickCmd(m.interval))
}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// fetchCmd samples every site and reads the host summary. This is the "real
// I/O" side of the Elm split: it runs in a goroutine bubbletea manages, and
// its result arrives back through Update as an ordinary message.
func (m Model) fetchCmd() tea.Cmd {
	sampler, provider := m.sampler, m.sites
	return func() tea.Msg {
		sites := provider.Sites()
		rows := sampler.Sample(context.Background(), sites)
		return statsMsg{rows: rows, host: readHostSummary()}
	}
}

// purgeCmd performs the actual purge. PurgeCache accepts a slug or a domain
// interchangeably (state.Find matches either), so the slug alone — already
// on hand from the selected row — is all this needs.
func (m Model) purgeCmd(slug string) tea.Cmd {
	purger := m.purger
	return func() tea.Msg {
		if purger == nil {
			return purgeDoneMsg{slug: slug, err: fmt.Errorf("no purger configured")}
		}
		err := purger.PurgeCache(slug)
		return purgeDoneMsg{slug: slug, err: err}
	}
}

// Update is the pure state transition every message flows through.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// table.SetHeight(h) stores h minus its own header's rendered height
		// as the visible row area — Height() afterward reports less than
		// what was passed in, not the same value. minTableInput already
		// accounts for that gap, so the *visible rows* floor this actually
		// produces is the minRows constant, not minTableInput itself.
		tableHeight := msg.Height - chromeHeight
		if tableHeight < minTableInput {
			tableHeight = minTableInput
		}
		m.table.SetHeight(tableHeight)
		m.table.SetWidth(msg.Width)
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.fetchCmd(), tickCmd(m.interval))

	case statsMsg:
		m.err = msg.err
		m.host = msg.host
		if msg.err == nil {
			m.rows = sortRows(msg.rows, m.sortBy, m.sortDesc)
			m.table.SetRows(rowsToTable(m.rows, m.purging))
		}
		return m, nil

	case purgeStartedMsg:
		m.purging[msg.slug] = true
		m.statusMsg = "purging cache for " + msg.slug + "…"
		// This is the step that was missing: setting the in-flight flag and
		// showing a status message is not the purge — without returning
		// purgeCmd here, "p" would mark a site as purging and then nothing
		// would ever actually call PurgeCache, or clear the flag, or tell
		// the operator it finished.
		return m, m.purgeCmd(msg.slug)

	case purgeDoneMsg:
		delete(m.purging, msg.slug)
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("purge failed for %s: %v", msg.slug, msg.err)
		} else {
			m.statusMsg = "cache purged for " + msg.slug
		}
		m.table.SetRows(rowsToTable(m.rows, m.purging))
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.quitting = true
		return m, tea.Quit

	case "c":
		return m.resort(sortByCPU), nil
	case "m":
		return m.resort(sortByMem), nil
	case "r":
		return m.resort(sortByReqs), nil
	case "d":
		return m.resort(sortByDomain), nil

	case "p":
		if len(m.rows) == 0 {
			return m, nil
		}
		i := m.table.Cursor()
		if i < 0 || i >= len(m.rows) {
			return m, nil
		}
		row := m.rows[i]
		if m.purging[row.Slug] {
			return m, nil // already in flight; don't queue a second purge
		}
		m.purging[row.Slug] = true
		return m, func() tea.Msg { return purgeStartedMsg{slug: row.Slug} }
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// resort toggles direction when the same key is pressed twice — the same
// gesture `sort` and every spreadsheet uses — and defaults to descending
// (busiest first) on a fresh key, since "what's eating my box" is almost
// always the question this dashboard exists to answer.
func (m Model) resort(key sortKey) Model {
	if m.sortBy == key {
		m.sortDesc = !m.sortDesc
	} else {
		m.sortBy = key
		m.sortDesc = true
	}
	m.rows = sortRows(m.rows, m.sortBy, m.sortDesc)
	m.table.SetColumns(columnsFor(m.sortBy, m.sortDesc))
	m.table.SetRows(rowsToTable(m.rows, m.purging))
	return m
}

func sortRows(rows []stats.SiteStats, by sortKey, desc bool) []stats.SiteStats {
	out := make([]stats.SiteStats, len(rows))
	copy(out, rows)
	less := func(i, j int) bool {
		switch by {
		case sortByMem:
			return out[i].MemoryMB < out[j].MemoryMB
		case sortByReqs:
			return out[i].ReqPerSec < out[j].ReqPerSec
		case sortByDomain:
			return out[i].Domain > out[j].Domain // reversed so "desc" (default) reads A→Z
		default:
			return out[i].CPUPercent < out[j].CPUPercent
		}
	}
	if desc {
		sort.SliceStable(out, func(i, j int) bool { return less(j, i) })
	} else {
		sort.SliceStable(out, less)
	}
	return out
}
