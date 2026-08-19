package security

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseYARAOutput(t *testing.T) {
	// Exact format observed from a real `yara -r` invocation against this
	// package's own bundled ruleset, not assumed from documentation.
	out := `eval_of_decoded_input /var/www/example-com/public/wp-content/uploads/shell1.php
shell_exec_of_request_input /var/www/example-com/public/wp-content/uploads/shell2.php
`
	findings := parseYARAOutput(out, "/var/www/example-com/public")
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(findings))
	}
	if findings[0].Path != "wp-content/uploads/shell1.php" {
		t.Errorf("Path = %q", findings[0].Path)
	}
	if findings[0].Severity != Critical {
		t.Errorf("Severity = %v, want Critical", findings[0].Severity)
	}
	if findings[0].Category != CategoryMalware {
		t.Errorf("Category = %v, want CategoryMalware", findings[0].Category)
	}
}

func TestParseYARAOutputIgnoresWarningsAndErrors(t *testing.T) {
	out := `warning: rule "long_base64_blob" in rules/php-malware.yar(138): string "$a" may slow down scanning
eval_of_decoded_input /path/shell.php
error: something unrelated
`
	findings := parseYARAOutput(out, "/path")
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1 (warning/error lines must not become findings)", len(findings))
	}
}

func TestParseYARAOutputCleanScan(t *testing.T) {
	if findings := parseYARAOutput("", "/x"); len(findings) != 0 {
		t.Errorf("expected no findings on a clean scan, got %v", findings)
	}
}

// An externally supplied rule (not in the bundled set) must still produce a
// usable, if more generic, finding rather than being dropped.
func TestParseYARAOutputUnknownRuleStillProducesAFinding(t *testing.T) {
	findings := parseYARAOutput("some_custom_external_rule /path/x.php\n", "/path")
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	if findings[0].Title == "" {
		t.Error("an unrecognised (externally supplied) rule name should still produce a labelled finding")
	}
}

func TestMaterializeRulesWritesEmbeddedRuleset(t *testing.T) {
	y := YARA{}
	dir, cleanup, err := y.materializeRules()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no rule files were materialized from the embedded ruleset")
	}
	found := false
	for _, e := range entries {
		if e.Name() == "php-malware.yar" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected php-malware.yar among materialized files, got %v", entries)
	}
}

// Cleanup must actually remove the temporary directory, or a long-running
// dashboard/scanner process leaks a new directory on every scan.
func TestMaterializeRulesCleanupRemovesDir(t *testing.T) {
	y := YARA{}
	dir, cleanup, err := y.materializeRules()
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected the temp rules directory to be removed, stat err = %v", err)
	}
}

// An external rules directory must be picked up without disabling the
// bundled rules, and must not be able to shadow a bundled rule by using the
// same filename.
func TestMaterializeRulesIncludesExtraDirWithoutShadowing(t *testing.T) {
	extraDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extraDir, "php-malware.yar"), []byte("rule fake { condition: false }"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extraDir, "custom.yar"), []byte("rule custom_rule { condition: false }"), 0o644); err != nil {
		t.Fatal(err)
	}

	y := YARA{ExtraRulesDir: extraDir}
	dir, cleanup, err := y.materializeRules()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	entries, _ := os.ReadDir(dir)
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["php-malware.yar"] {
		t.Error("bundled ruleset was not written")
	}
	if !names["extra-custom.yar"] {
		t.Error("external ruleset file was not included")
	}
	// The bundled file must survive under its own name — an external file
	// with the same name is prefixed, not allowed to overwrite it.
	bundled, err := os.ReadFile(filepath.Join(dir, "php-malware.yar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(bundled) == "rule fake { condition: false }" {
		t.Error("an external rules file was allowed to shadow/overwrite the bundled ruleset")
	}
}

func TestMaterializeRulesToleratesMissingExtraDir(t *testing.T) {
	y := YARA{ExtraRulesDir: "/does/not/exist"}
	dir, cleanup, err := y.materializeRules()
	if err != nil {
		t.Fatalf("a missing extra rules dir should degrade gracefully, got %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(dir, "php-malware.yar")); err != nil {
		t.Error("bundled rules should still be present even when the extra dir is missing")
	}
}

// This is the strongest check available: actually invoke the real yara
// binary (skipped if not installed) against the bundled ruleset and confirm
// it compiles cleanly and matches a known-bad sample — the same validation
// this ruleset was authored and hand-verified against.
func TestBundledRulesetCompilesAndMatchesWithRealYARA(t *testing.T) {
	if _, err := exec.LookPath("yara"); err != nil {
		t.Skip("yara is not installed on this machine; skipping live compile/match check")
	}

	y := YARA{Runner: realRunner{}}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "shell.php"),
		[]byte(`<?php eval(base64_decode($_POST['z'])); ?>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "clean.php"),
		[]byte(`<?php echo "hello"; ?>`), 0o644); err != nil {
		t.Fatal(err)
	}

	findings, err := y.Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range findings {
		if f.Path == "shell.php" {
			found = true
		}
	}
	if !found {
		t.Errorf("real yara did not flag the known-malicious sample; findings = %v", findings)
	}
	for _, f := range findings {
		if f.Path == "clean.php" {
			t.Errorf("real yara flagged a clean file: %v", f)
		}
	}
}

// This is the exact false positive found live-testing against a real
// WordPress installation: unmodified wp-includes/embed.php and the bundled
// Plupload upload widget both legitimately use a hidden iframe (embed.php's
// oEmbed response carries marginwidth="0"/marginheight="0"; Plupload's
// upload fallback opens a display:none iframe), which the removed
// hidden_iframe_injection rule matched on every single WordPress install
// regardless of compromise. These exact snippets are pinned here so that
// rule — or anything shaped like it — cannot silently come back without this
// test catching it.
func TestBundledRulesetDoesNotFlagKnownWordPressCorePatterns(t *testing.T) {
	if _, err := exec.LookPath("yara"); err != nil {
		t.Skip("yara is not installed on this machine; skipping live compile/match check")
	}

	samples := map[string]string{
		"wp-includes/embed.php oEmbed iframe": `<iframe sandbox="allow-scripts" security="restricted" src="%1$s" width="%2$d" height="%3$d" title="%4$s" data-secret="%5$s" frameborder="0" marginwidth="0" marginheight="0" scrolling="no" class="wp-embedded-content"></iframe>`,
		"moxie.js upload fallback iframe":     `<iframe id="' + uid + '_iframe" name="' + uid + '_iframe" src="javascript:&quot;&quot;" style="display:none"></iframe>`,
	}

	y := YARA{Runner: realRunner{}}
	for name, content := range samples {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "sample.php"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			findings, err := y.Scan(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != 0 {
				t.Errorf("real WordPress-core pattern %q produced false positive(s): %v", name, findings)
			}
		})
	}
}

// realRunner shells out for real — used only by the skippable live-yara test
// above, which needs actual command execution rather than a fake.
type realRunner struct{}

func (realRunner) Look(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
func (realRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}
func (realRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}
