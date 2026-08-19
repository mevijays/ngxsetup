package security

import (
	"context"
	"fmt"
)

// Scanner ties every detection layer together against one site.
type Scanner struct {
	Runner Runner
	// YARARulesDir supplements the bundled ruleset — see YARA.ExtraRulesDir.
	YARARulesDir string
}

// Target describes the one site being scanned.
type Target struct {
	Domain string
	User   string // system account the site's PHP-FPM pool runs as
	Root   string // document root
}

// Scan runs every available layer against target and returns a combined
// Report. No layer's absence is fatal — a machine with none of the optional
// tools installed still gets the built-in heuristic scan and, if wp-cli is
// present, checksum verification, and the report says plainly which of the
// stronger layers it could not run.
func (s Scanner) Scan(ctx context.Context, target Target) (*Report, error) {
	report := &Report{Domain: target.Domain, LayersSkipped: map[string]string{}}

	// Layer 1: built-in heuristics. Always runs.
	findings, err := WalkAndScan(target.Root)
	if err != nil {
		return nil, fmt.Errorf("scanning %s: %w", target.Root, err)
	}
	report.Findings = append(report.Findings, findings...)
	report.LayersRun = append(report.LayersRun, "heuristics")

	// Layer 2: wp-cli checksum verification and account/version audit.
	//
	// Each wp-cli-backed check is recorded under its own key in either
	// LayersRun or LayersSkipped, and consistently so regardless of *why* it
	// did not run — wp-cli missing entirely and one specific wp-cli call
	// failing must both be visible to a caller checking a specific layer's
	// name, not just the coarser "wp-cli is unavailable" case.
	wp := WPCLI{Runner: s.Runner, User: target.User, Path: target.Root}
	if !wp.Available() {
		for _, layer := range []string{"wp-cli core checksums", "wp-cli plugin checksums", "admin account audit"} {
			report.LayersSkipped[layer] = "wp-cli is not installed"
		}
	} else {
		if findings, err := wp.VerifyCoreChecksums(ctx); err == nil {
			report.Findings = append(report.Findings, findings...)
			report.LayersRun = append(report.LayersRun, "wp-cli core checksums")
		} else {
			report.LayersSkipped["wp-cli core checksums"] = err.Error()
		}

		pluginFindings, uncheckable, err := wp.VerifyPluginChecksums(ctx)
		if err == nil {
			report.Findings = append(report.Findings, pluginFindings...)
			report.LayersRun = append(report.LayersRun, "wp-cli plugin checksums")
			if uncheckable > 0 {
				report.Add(Info, CategoryIntegrity, "some plugins could not be checksum-verified",
					fmt.Sprintf("%d plugin(s) are not distributed through wordpress.org, so no official checksum exists to compare against", uncheckable),
					"", "review these plugins' provenance manually; checksum verification only covers wordpress.org-hosted code")
			}
		} else {
			report.LayersSkipped["wp-cli plugin checksums"] = err.Error()
		}

		if admins, err := wp.AdminUsers(ctx); err == nil {
			report.LayersRun = append(report.LayersRun, "admin account audit")
			report.Add(Info, CategoryAccount, "administrator accounts",
				fmt.Sprintf("%d account(s) with the administrator role: review that every one is expected", len(admins)),
				"", "wp user list --role=administrator")
		}
	}

	// Layer 3: ClamAV, if installed.
	clam := ClamAV{Runner: s.Runner}
	if !clam.Available() {
		report.LayersSkipped["ClamAV"] = "clamscan is not installed (optional: apt install clamav-daemon)"
	} else {
		findings, err := clam.Scan(ctx, target.Root)
		if err != nil {
			report.LayersSkipped["ClamAV"] = err.Error()
		} else {
			report.Findings = append(report.Findings, findings...)
			report.LayersRun = append(report.LayersRun, "ClamAV")
		}
	}

	// Layer 4: YARA, if installed.
	yara := YARA{Runner: s.Runner, ExtraRulesDir: s.YARARulesDir}
	if !yara.Available() {
		report.LayersSkipped["YARA"] = "yara is not installed (optional: apt install yara)"
	} else {
		findings, err := yara.Scan(ctx, target.Root)
		if err != nil {
			report.LayersSkipped["YARA"] = err.Error()
		} else {
			report.Findings = append(report.Findings, findings...)
			report.LayersRun = append(report.LayersRun, "YARA")
		}
	}

	return report, nil
}
