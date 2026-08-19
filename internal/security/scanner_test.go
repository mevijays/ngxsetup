package security

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A machine with none of the optional tools installed must still produce a
// useful report from the built-in heuristics alone — that is the whole point
// of layering detection rather than requiring every tool to be present.
func TestScanWithNothingInstalledStillFindsHeuristics(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "shell.php"), []byte(`<?php eval($_POST['x']); ?>`), 0o644)

	s := Scanner{Runner: fakeWPRunner{installed: false}}
	report, err := s.Scan(context.Background(), Target{Domain: "example.com", User: "web-example-com", Root: root})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, f := range report.Findings {
		if f.Category == CategoryHeuristic {
			found = true
		}
	}
	if !found {
		t.Error("expected the heuristic layer to still find the eval() backdoor")
	}

	for _, layer := range []string{"wp-cli core checksums", "ClamAV", "YARA"} {
		if _, skipped := report.LayersSkipped[layer]; !skipped {
			t.Errorf("%s should be recorded as skipped when its tool is not installed", layer)
		}
	}
	if len(report.LayersRun) != 1 || report.LayersRun[0] != "heuristics" {
		t.Errorf("LayersRun = %v, want exactly [heuristics]", report.LayersRun)
	}
}

// A report must say which layers it actually ran, not just what it found —
// two scans that found nothing are not the same statement of confidence if
// one only had heuristics and the other had every layer.
func TestScanRecordsWhichLayersActuallyRan(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "index.php"), []byte(`<?php echo "hi"; ?>`), 0o644)

	runner := fakeWPRunner{installed: true, responses: map[string]fakeResponse{
		"core verify-checksums":   {out: ""},
		"plugin verify-checksums": {out: ""},
		"user list":               {out: "user_login\nadmin\n"},
	}}
	s := Scanner{Runner: runner}
	report, err := s.Scan(context.Background(), Target{Domain: "example.com", User: "web-example-com", Root: root})
	if err != nil {
		t.Fatal(err)
	}

	wantLayers := map[string]bool{
		"heuristics": false, "wp-cli core checksums": false,
		"wp-cli plugin checksums": false, "admin account audit": false,
	}
	for _, l := range report.LayersRun {
		wantLayers[l] = true
	}
	for layer, ran := range wantLayers {
		if !ran {
			t.Errorf("expected %q to have run, LayersRun = %v", layer, report.LayersRun)
		}
	}
}

// The uncheckable-plugin count (non-.org plugins) must surface as an
// explicit informational finding — silently treating "could not check" the
// same as "checked and clean" is exactly the kind of false confidence a
// security tool must not produce.
func TestScanReportsUncheckablePlugins(t *testing.T) {
	root := t.TempDir()
	runner := fakeWPRunner{installed: true, responses: map[string]fakeResponse{
		"core verify-checksums": {out: ""},
		"plugin verify-checksums": {out: "The plugin premium-plugin is not installed from wordpress.org, so its " +
			"checksums cannot be verified.\n"},
		"user list": {out: "user_login\n"},
	}}
	s := Scanner{Runner: runner}
	report, err := s.Scan(context.Background(), Target{Domain: "x", User: "u", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range report.Findings {
		if strings.Contains(f.Title, "could not be checksum-verified") {
			found = true
		}
	}
	if !found {
		t.Error("expected an explicit finding noting that some plugins could not be checksum-verified")
	}
}

func TestReportCleanAndCountBySeverity(t *testing.T) {
	var r Report
	r.Add(Info, CategoryAccount, "t", "d", "", "")
	c, w, i := r.CountBySeverity()
	if c != 0 || w != 0 || i != 1 {
		t.Errorf("counts = %d/%d/%d, want 0/0/1", c, w, i)
	}
	if !r.Clean() {
		t.Error("a report with only Info findings should report Clean")
	}

	r.Add(Warning, CategoryHeuristic, "t", "d", "", "")
	if r.Clean() {
		t.Error("a report with a Warning finding should not report Clean")
	}
}
