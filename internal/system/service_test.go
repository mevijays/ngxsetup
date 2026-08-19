package system

import "testing"

// systemd's list-unit-files enumerates only the template (svc@.service),
// never its instances, so UnitExists has to branch on this. Getting the
// classification wrong meant every running per-site service was reported as
// missing — confirmed live before this was fixed.
func TestIsTemplateInstance(t *testing.T) {
	instances := []string{
		"ngxsetup-fpm@alpha-example-com.service",
		"getty@tty1.service",
		"svc@x",
	}
	for _, u := range instances {
		if !isTemplateInstance(u) {
			t.Errorf("isTemplateInstance(%q) = false, want true", u)
		}
	}

	notInstances := []string{
		"nginx.service",
		"mariadb.service",
		// The bare template itself is not an instance: list-unit-files does
		// know about this one, so it must take the ordinary path.
		"ngxsetup-fpm@.service",
		"svc@",
		"",
	}
	for _, u := range notInstances {
		if isTemplateInstance(u) {
			t.Errorf("isTemplateInstance(%q) = true, want false", u)
		}
	}
}
