package security

import "testing"

func TestParseClamAVOutput(t *testing.T) {
	out := `/var/www/example-com/public/wp-content/uploads/shell.php: Php.Trojan.WebShell-1 FOUND
/var/www/example-com/public/index.php: OK
/var/www/example-com/public/wp-includes/backdoor.php: Php.Backdoor.Generic-42 FOUND
`
	findings := parseClamAVOutput(out, "/var/www/example-com/public")
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2 (OK lines must not produce findings)", len(findings))
	}
	if findings[0].Path != "wp-content/uploads/shell.php" {
		t.Errorf("Path = %q", findings[0].Path)
	}
	if findings[0].Detail != "matches known-malware signature Php.Trojan.WebShell-1" {
		t.Errorf("Detail = %q", findings[0].Detail)
	}
	if findings[0].Severity != Critical {
		t.Errorf("Severity = %v, want Critical", findings[0].Severity)
	}
	if findings[1].Path != "wp-includes/backdoor.php" {
		t.Errorf("Path = %q", findings[1].Path)
	}
}

func TestParseClamAVOutputCleanScan(t *testing.T) {
	out := "" // --no-summary -i with nothing infected produces no lines at all
	if findings := parseClamAVOutput(out, "/x"); len(findings) != 0 {
		t.Errorf("expected no findings on a clean scan, got %v", findings)
	}
}

func TestParseClamAVOutputIgnoresMalformedLines(t *testing.T) {
	out := "this is not a clamscan line\nFOUND\n: FOUND\n"
	findings := parseClamAVOutput(out, "/x")
	if len(findings) != 0 {
		t.Errorf("malformed lines should be skipped, not produce garbage findings, got %v", findings)
	}
}
