package state

import (
	"path/filepath"
	"testing"
)

func load(t *testing.T, path string) *State {
	t.Helper()
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func slugs(s *State) []string {
	var out []string
	for _, site := range s.Sites {
		out = append(out, site.Slug)
	}
	return out
}

// Two holders of the registry — a long-running migration and the web UI,
// say — must not erase each other's changes when both save.
func TestConcurrentSavesKeepEachOthersChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	seed := load(t, path)
	seed.Upsert(Site{Slug: "keep", Domain: "keep.example"})
	seed.Upsert(Site{Slug: "gone", Domain: "gone.example"})
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	a := load(t, path) // e.g. a migration job
	b := load(t, path) // e.g. the web UI

	a.Upsert(Site{Slug: "migrated", Domain: "migrated.example"})
	b.Upsert(Site{Slug: "added", Domain: "added.example"})
	b.Delete("gone")

	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	got := slugs(load(t, path))
	want := []string{"added", "keep", "migrated"}
	if len(got) != len(want) {
		t.Fatalf("sites after both saves = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sites after both saves = %v, want %v", got, want)
		}
	}
	// The saver's in-memory copy catches up with what is on disk.
	if len(a.Sites) != 3 {
		t.Errorf("in-memory state after save has %v, want the merged 3 sites", slugs(a))
	}
}

// A change made through a pointer from Find (SetEnabled does this) counts as
// a change too.
func TestSaveKeepsInPlaceEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	seed := load(t, path)
	seed.Upsert(Site{Slug: "one", Domain: "one.example", Enabled: true})
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	a := load(t, path)
	b := load(t, path)
	rec, err := a.Find("one")
	if err != nil {
		t.Fatal(err)
	}
	rec.Enabled = false
	b.Upsert(Site{Slug: "two", Domain: "two.example"})
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	final := load(t, path)
	one, err := final.Find("one")
	if err != nil || one.Enabled {
		t.Errorf("the disable made through Find was lost: %+v, %v", one, err)
	}
	if _, err := final.Find("two"); err != nil {
		t.Error("the other writer's new site was lost")
	}
}

// Saving without having touched a site must not revert another writer's
// newer version of it.
func TestSaveDoesNotRevertOthersEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	seed := load(t, path)
	seed.Upsert(Site{Slug: "one", Domain: "one.example"})
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	stale := load(t, path)
	fresh := load(t, path)
	rec, _ := fresh.Find("one")
	rec.TLS = true
	if err := fresh.Save(); err != nil {
		t.Fatal(err)
	}

	stale.Touch()
	if err := stale.Save(); err != nil {
		t.Fatal(err)
	}
	one, _ := load(t, path).Find("one")
	if !one.TLS {
		t.Error("a stale holder's unrelated save reverted another writer's change")
	}
}
