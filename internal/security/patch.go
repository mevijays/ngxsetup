package security

import (
	"context"
	"fmt"
)

// PatchPlan is what a patch operation would change, gathered up front so an
// operator (or the CLI's confirmation prompt) can see the whole picture
// before anything is actually touched.
type PatchPlan struct {
	Domain string
	// CoreCurrentVersion is WordPress's currently installed version,
	// always populated — display context for CoreUpdate, which is only
	// ever the target version.
	CoreCurrentVersion string
	CoreUpdate         string // target version, or "" if core is already current
	Plugins            []wpItem
	Themes             []wpItem
}

// Empty reports whether there is nothing to patch.
func (p PatchPlan) Empty() bool {
	return p.CoreUpdate == "" && len(p.Plugins) == 0 && len(p.Themes) == 0
}

// Describe renders the plan as human-readable lines, in the same spirit as
// tuning.Plan.Explain() — an operator approving an update should see exactly
// what it will do, not take it on faith.
func (p PatchPlan) Describe() []string {
	var lines []string
	if p.CoreUpdate != "" {
		lines = append(lines, fmt.Sprintf("WordPress core -> %s", p.CoreUpdate))
	}
	for _, pl := range p.Plugins {
		lines = append(lines, fmt.Sprintf("plugin %s -> update available", pl.Name))
	}
	for _, th := range p.Themes {
		lines = append(lines, fmt.Sprintf("theme %s -> update available", th.Name))
	}
	return lines
}

// Select builds a new plan containing only the chosen items — the
// operator's checkbox selection in the web UI, or a scripted subset from
// the CLI — so ApplyPatch only ever touches what was actually approved,
// not everything PlanPatch happened to find outdated. Names not present in
// the original plan are silently ignored rather than erroring: the plan
// this is filtering came from the same wp-cli query moments earlier, so a
// name that no longer matches almost always means the operator's browser
// tab was open a while and something changed underneath it, not a typo
// worth failing the whole patch over.
func (p PatchPlan) Select(core bool, pluginNames, themeNames []string) PatchPlan {
	out := PatchPlan{Domain: p.Domain, CoreCurrentVersion: p.CoreCurrentVersion}
	if core {
		out.CoreUpdate = p.CoreUpdate
	}
	want := func(names []string) map[string]bool {
		m := make(map[string]bool, len(names))
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	wantPlugins, wantThemes := want(pluginNames), want(themeNames)
	for _, pl := range p.Plugins {
		if wantPlugins[pl.Name] {
			out.Plugins = append(out.Plugins, pl)
		}
	}
	for _, th := range p.Themes {
		if wantThemes[th.Name] {
			out.Themes = append(out.Themes, th)
		}
	}
	return out
}

// PlanPatch gathers what is outdated without changing anything.
func (w WPCLI) PlanPatch(ctx context.Context, domain string) (*PatchPlan, error) {
	if !w.Available() {
		return nil, fmt.Errorf("wp-cli is not installed")
	}
	plan := &PatchPlan{Domain: domain}

	current, err := w.CoreVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the installed core version: %w", err)
	}
	plan.CoreCurrentVersion = current

	core, err := w.CoreUpdateAvailable(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking core version: %w", err)
	}
	plan.CoreUpdate = core

	plugins, err := w.OutdatedPlugins(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking plugin versions: %w", err)
	}
	plan.Plugins = plugins

	themes, err := w.OutdatedThemes(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking theme versions: %w", err)
	}
	plan.Themes = themes

	return plan, nil
}

// PatchResult records what actually happened, since a real run can partially
// succeed — a plugin update failing must not be reported the same as every
// update failing, and must not stop the rest of the plan from being
// attempted.
type PatchResult struct {
	CoreUpdated    bool
	CoreErr        error
	PluginsUpdated []string
	PluginErrs     map[string]error
	ThemesUpdated  []string
	ThemeErrs      map[string]error
}

// Success reports whether every part of the plan that was attempted
// succeeded.
func (r PatchResult) Success() bool {
	return r.CoreErr == nil && len(r.PluginErrs) == 0 && len(r.ThemeErrs) == 0
}

// ApplyPatch executes a previously reviewed plan. Core is updated first —
// plugin and theme compatibility issues are far more common against a
// mismatched core version than the other way around — and one item's
// failure does not stop the rest of the plan from being attempted, since an
// operator who approved five updates should get four applied rather than
// zero because the first one failed.
func (w WPCLI) ApplyPatch(ctx context.Context, plan *PatchPlan) PatchResult {
	result := PatchResult{
		PluginErrs: map[string]error{},
		ThemeErrs:  map[string]error{},
	}

	if plan.CoreUpdate != "" {
		if _, err := w.run(ctx, "core", "update"); err != nil {
			result.CoreErr = err
		} else {
			result.CoreUpdated = true
		}
	}

	for _, p := range plan.Plugins {
		if _, err := w.run(ctx, "plugin", "update", p.Name); err != nil {
			result.PluginErrs[p.Name] = err
			continue
		}
		result.PluginsUpdated = append(result.PluginsUpdated, p.Name)
	}

	for _, th := range plan.Themes {
		if _, err := w.run(ctx, "theme", "update", th.Name); err != nil {
			result.ThemeErrs[th.Name] = err
			continue
		}
		result.ThemesUpdated = append(result.ThemesUpdated, th.Name)
	}

	return result
}
