package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"ngxsetup/internal/stats"
)

// fakeSites and fakePurger let Update's logic be tested without a real
// server — bubbletea models are exercised by feeding tea.Msg values into
// Update and inspecting the resulting Model, the same pattern the framework
// itself is tested with.
type fakeSites struct{ sites []stats.Site }

func (f fakeSites) Sites() []stats.Site { return f.sites }

type fakePurger struct {
	calls []string
	err   error
}

func (f *fakePurger) PurgeCache(domain string) error {
	f.calls = append(f.calls, domain)
	return f.err
}

func sampleRows() []stats.SiteStats {
	return []stats.SiteStats{
		{Slug: "a-com", Domain: "a.com", CPUPercent: 10, MemoryMB: 200, ReqPerSec: 1, MaxWorkers: 10},
		{Slug: "b-com", Domain: "b.com", CPUPercent: 90, MemoryMB: 50, ReqPerSec: 5, MaxWorkers: 10},
		{Slug: "c-com", Domain: "c.com", CPUPercent: 40, MemoryMB: 900, ReqPerSec: 3, MaxWorkers: 10},
	}
}

func TestUpdateAppliesStatsAndDefaultSortIsCPUDescending(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	next, _ := m.Update(statsMsg{rows: sampleRows()})
	got := next.(Model)

	if len(got.rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(got.rows))
	}
	// b.com has the highest CPU (90) and must sort first by default.
	if got.rows[0].Domain != "b.com" {
		t.Errorf("first row = %s, want b.com (highest CPU, default sort)", got.rows[0].Domain)
	}
	if got.rows[2].Domain != "a.com" {
		t.Errorf("last row = %s, want a.com (lowest CPU)", got.rows[2].Domain)
	}
}

// A sample that errors must not wipe out the previously good rows — a
// dashboard should not go blank because one tick's fetch hiccuped.
func TestUpdatePreservesRowsOnFetchError(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	next, _ := m.Update(statsMsg{rows: sampleRows()})
	m = next.(Model)

	next, _ = m.Update(statsMsg{err: errBoom})
	got := next.(Model)
	if len(got.rows) != 3 {
		t.Errorf("rows were cleared on a fetch error; got %d, want 3 preserved", len(got.rows))
	}
	if got.err == nil {
		t.Error("the error should still be recorded for the footer to show")
	}
}

func TestKeyPressResortsByMemory(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	next, _ := m.Update(statsMsg{rows: sampleRows()})
	m = next.(Model)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	got := next.(Model)

	if got.sortBy != sortByMem {
		t.Fatalf("sortBy = %v, want sortByMem", got.sortBy)
	}
	if got.rows[0].Domain != "c.com" {
		t.Errorf("first row after sorting by memory = %s, want c.com (900MB, highest)", got.rows[0].Domain)
	}
}

// Pressing the same sort key twice must flip direction — the universal
// spreadsheet gesture — rather than doing nothing or resetting to descending
// every time. Starts from a memory sort (not the CPU default the model
// already opens in) so the first press below is unambiguously "select req/s
// sort," not itself a toggle of a sort the model was already showing.
func TestKeyPressTwiceTogglesSortDirection(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	next, _ := m.Update(statsMsg{rows: sampleRows()})
	m = next.(Model)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(Model)
	if m.sortBy != sortByReqs || !m.sortDesc {
		t.Fatalf("first press should select req/s sort, descending; got sortBy=%v desc=%v", m.sortBy, m.sortDesc)
	}
	if m.rows[0].Domain != "b.com" {
		t.Errorf("descending req/s should put b.com (5/s, highest) first, got %s", m.rows[0].Domain)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(Model)
	if m.sortDesc {
		t.Fatal("second press of the same sort key should flip to ascending")
	}
	if m.rows[0].Domain != "a.com" {
		t.Errorf("ascending req/s sort should put a.com (1/s, lowest) first, got %s", m.rows[0].Domain)
	}
}

// The already-default case matters too: the model opens sorted by CPU
// descending, so a *first* press of "c" is indistinguishable from a second
// press of a key that was already selected — it must toggle to ascending,
// not silently do nothing.
func TestKeyPressOnAlreadyActiveSortToggles(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	next, _ := m.Update(statsMsg{rows: sampleRows()})
	m = next.(Model)
	if m.sortBy != sortByCPU || !m.sortDesc {
		t.Fatal("test assumption broken: model should open sorted by CPU descending")
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	m = next.(Model)
	if m.sortDesc {
		t.Error("pressing the already-active sort key must toggle direction, not no-op")
	}
	if m.rows[0].Domain != "a.com" {
		t.Errorf("ascending CPU sort should put a.com (10%%, lowest) first, got %s", m.rows[0].Domain)
	}
}

func TestQuitKeyReturnsQuitCommand(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestCtrlCAlsoQuits(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
}

// The 'p' key must trigger a purge for whichever row the cursor is on, not
// always the first row — fat-fingering the wrong site's cache is exactly the
// kind of mistake a dashboard should not make easy.
func TestPurgeKeyTargetsSelectedRow(t *testing.T) {
	purger := &fakePurger{}
	m := New(fakeSites{}, nil, purger, 0)
	next, _ := m.Update(statsMsg{rows: sampleRows()})
	m = next.(Model)

	m.table.SetCursor(1) // second row after CPU-descending sort: c.com (40%)
	selectedSlug := m.rows[1].Slug

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	m = next.(Model)
	if !m.purging[selectedSlug] {
		t.Fatalf("expected %s to be marked purging, got %v", selectedSlug, m.purging)
	}
	if cmd == nil {
		t.Fatal("expected a command to kick off the purgeStartedMsg")
	}
	msg := cmd()
	started, ok := msg.(purgeStartedMsg)
	if !ok || started.slug != selectedSlug {
		t.Errorf("purgeStartedMsg = %+v, want slug %s", msg, selectedSlug)
	}
}

// A second 'p' press while a purge is already in flight for that site must
// not queue a duplicate — nginx only needs telling once.
func TestPurgeKeyIgnoredWhileAlreadyPurging(t *testing.T) {
	m := New(fakeSites{}, nil, &fakePurger{}, 0)
	next, _ := m.Update(statsMsg{rows: sampleRows()})
	m = next.(Model)
	m.purging[m.rows[0].Slug] = true

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd != nil {
		t.Error("expected no command when a purge is already in flight for the selected site")
	}
}

// This is the bug found while building this: setting the in-flight flag and
// showing "purging…" is not the same as the purge actually happening.
// purgeStartedMsg's handler must return the command that calls PurgeCache —
// without it, every purge would hang forever showing "purging…" with the
// cache never actually cleared.
func TestPurgeStartedMsgActuallyInvokesPurgeCache(t *testing.T) {
	purger := &fakePurger{}
	m := New(fakeSites{}, nil, purger, 0)

	_, cmd := m.Update(purgeStartedMsg{slug: "a-com"})
	if cmd == nil {
		t.Fatal("purgeStartedMsg produced no command; the purge would never actually run")
	}
	msg := cmd()
	done, ok := msg.(purgeDoneMsg)
	if !ok {
		t.Fatalf("expected purgeDoneMsg from the command, got %T", msg)
	}
	if done.slug != "a-com" {
		t.Errorf("purgeDoneMsg.slug = %q, want a-com", done.slug)
	}
	if len(purger.calls) != 1 || purger.calls[0] != "a-com" {
		t.Errorf("PurgeCache calls = %v, want exactly one call for a-com", purger.calls)
	}
}

func TestPurgeDoneClearsInFlightState(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	next, _ := m.Update(statsMsg{rows: sampleRows()})
	m = next.(Model)
	m.purging["a-com"] = true

	next, _ = m.Update(purgeDoneMsg{slug: "a-com"})
	got := next.(Model)
	if got.purging["a-com"] {
		t.Error("purging flag was not cleared after purgeDoneMsg")
	}
	if got.statusMsg == "" {
		t.Error("expected a status message reporting the purge result")
	}
}

func TestWindowResizeAdjustsTableDimensions(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	got := next.(Model)
	if got.width != 100 || got.height != 40 {
		t.Errorf("width/height = %d/%d, want 100/40", got.width, got.height)
	}
	if got.table.Height() < 3 {
		t.Errorf("table height = %d, want a sane positive minimum", got.table.Height())
	}
}

// A very small terminal must not produce a negative or zero table height,
// which bubbles/table treats as a rendering error.
func TestWindowResizeClampsSmallTerminal(t *testing.T) {
	m := New(fakeSites{}, nil, nil, 0)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	got := next.(Model)
	if got.table.Height() < 3 {
		t.Errorf("table height = %d, want >= 3 even on a tiny terminal", got.table.Height())
	}
}

var errBoom = boomError{}

type boomError struct{}

func (boomError) Error() string { return "boom" }
