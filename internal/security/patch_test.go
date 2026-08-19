package security

import (
	"context"
	"strings"
	"testing"
)

func TestPlanPatchGathersEverything(t *testing.T) {
	w := WPCLI{
		Runner: fakeWPRunner{installed: true, responses: map[string]fakeResponse{
			"core check-update":              {out: `[{"version":"6.7.1"}]`},
			"plugin list --update=available": {out: `[{"name":"akismet"},{"name":"jetpack"}]`},
			"theme list --update=available":  {out: `[{"name":"twentytwentyfive"}]`},
		}},
		User: "u", Path: "/x",
	}
	plan, err := w.PlanPatch(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if plan.CoreUpdate != "6.7.1" {
		t.Errorf("CoreUpdate = %q, want 6.7.1", plan.CoreUpdate)
	}
	if len(plan.Plugins) != 2 || len(plan.Themes) != 1 {
		t.Errorf("plan = %+v, want 2 plugins and 1 theme", plan)
	}
	if plan.Empty() {
		t.Error("a plan with pending updates must not report Empty")
	}
}

func TestPlanPatchEmptyWhenEverythingCurrent(t *testing.T) {
	w := WPCLI{
		Runner: fakeWPRunner{installed: true, responses: map[string]fakeResponse{
			"core check-update":              {out: `[]`},
			"plugin list --update=available": {out: `[]`},
			"theme list --update=available":  {out: `[]`},
		}},
		User: "u", Path: "/x",
	}
	plan, err := w.PlanPatch(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Errorf("expected an empty plan, got %+v", plan)
	}
}

func TestPlanPatchRequiresWPCLI(t *testing.T) {
	w := WPCLI{Runner: fakeWPRunner{installed: false}, User: "u", Path: "/x"}
	if _, err := w.PlanPatch(context.Background(), "example.com"); err == nil {
		t.Error("expected an error when wp-cli is not installed")
	}
}

func TestPatchPlanDescribe(t *testing.T) {
	plan := PatchPlan{
		CoreUpdate: "6.7.1",
		Plugins:    []wpItem{{Name: "akismet"}},
		Themes:     []wpItem{{Name: "twentytwentyfive"}},
	}
	lines := plan.Describe()
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"6.7.1", "akismet", "twentytwentyfive"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Describe() output missing %q:\n%s", want, joined)
		}
	}
}

// A plugin update failing must not stop the rest of the plan — an operator
// who approved five updates should get the other four applied, not zero
// because the first one failed and the loop gave up.
func TestApplyPatchContinuesAfterOneFailure(t *testing.T) {
	w := WPCLI{
		Runner: fakeWPRunner{installed: true, responses: map[string]fakeResponse{
			"plugin update broken-plugin": {err: sentinelErr{"update failed: broken-plugin"}},
			"plugin update akismet":       {out: "Updated"},
		}},
		User: "u", Path: "/x",
	}
	plan := &PatchPlan{
		Plugins: []wpItem{{Name: "broken-plugin"}, {Name: "akismet"}},
	}
	result := w.ApplyPatch(context.Background(), plan)

	if len(result.PluginsUpdated) != 1 || result.PluginsUpdated[0] != "akismet" {
		t.Errorf("PluginsUpdated = %v, want [akismet]", result.PluginsUpdated)
	}
	if _, failed := result.PluginErrs["broken-plugin"]; !failed {
		t.Error("expected broken-plugin to be recorded as failed")
	}
	if result.Success() {
		t.Error("Success() should be false when one plugin failed")
	}
}

func TestApplyPatchCoreFirst(t *testing.T) {
	var order []string
	runner := recordingRunner{fn: func(args []string) {
		order = append(order, strings.Join(args, " "))
	}}
	w := WPCLI{Runner: runner, User: "u", Path: "/x"}
	plan := &PatchPlan{
		CoreUpdate: "6.7.1",
		Plugins:    []wpItem{{Name: "akismet"}},
		Themes:     []wpItem{{Name: "twentytwentyfive"}},
	}
	w.ApplyPatch(context.Background(), plan)

	if len(order) != 3 {
		t.Fatalf("got %d commands, want 3", len(order))
	}
	if !strings.Contains(order[0], "core update") {
		t.Errorf("first command = %q, want core update to run first", order[0])
	}
}

func TestApplyPatchEmptyPlanDoesNothing(t *testing.T) {
	var called bool
	runner := recordingRunner{fn: func(args []string) { called = true }}
	w := WPCLI{Runner: runner, User: "u", Path: "/x"}
	result := w.ApplyPatch(context.Background(), &PatchPlan{})
	if called {
		t.Error("an empty plan should not run any wp-cli command")
	}
	if !result.Success() {
		t.Error("an empty plan should trivially succeed")
	}
}

func TestPatchPlanSelectFiltersToChosenItems(t *testing.T) {
	full := PatchPlan{
		Domain: "example.com", CoreCurrentVersion: "6.7", CoreUpdate: "6.7.1",
		Plugins: []wpItem{{Name: "akismet"}, {Name: "jetpack"}},
		Themes:  []wpItem{{Name: "twentytwentyfive"}, {Name: "storefront"}},
	}

	selected := full.Select(true, []string{"jetpack"}, nil)
	if selected.CoreUpdate != "6.7.1" {
		t.Errorf("core = %q, want it included when core=true", selected.CoreUpdate)
	}
	if len(selected.Plugins) != 1 || selected.Plugins[0].Name != "jetpack" {
		t.Errorf("plugins = %+v, want exactly [jetpack]", selected.Plugins)
	}
	if len(selected.Themes) != 0 {
		t.Errorf("themes = %+v, want none selected", selected.Themes)
	}

	noneSelected := full.Select(false, nil, nil)
	if !noneSelected.Empty() {
		t.Errorf("selecting nothing must produce an empty plan, got %+v", noneSelected)
	}

	// A name the current plan does not contain (stale browser tab,
	// something changed underneath it) is silently dropped, not an error.
	stale := full.Select(false, []string{"does-not-exist"}, nil)
	if len(stale.Plugins) != 0 {
		t.Errorf("an unknown plugin name leaked into the filtered plan: %+v", stale.Plugins)
	}
}

type recordingRunner struct{ fn func([]string) }

func (r recordingRunner) Look(string) bool { return true }
func (r recordingRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	r.fn(args)
	return "", nil
}
func (r recordingRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	r.fn(args)
	return "", nil
}
