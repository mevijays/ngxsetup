// Package build holds the handful of values that identify this binary:
// its version and where it comes from.
//
// It exists as its own tiny package, rather than living in internal/cli
// alongside the rest of the command surface, because internal/cli imports
// internal/webui (for `ngxsetup web`) — so internal/webui cannot import
// internal/cli back without a cycle, and both need these values: the CLI's
// `version` command and help banner, and the web UI's footer credit. A
// package one level below both is the simplest fix.
package build

// Version is set at release build time via
// -ldflags "-X ngxsetup/internal/build.Version=vX.Y.Z" (see
// .github/workflows/release.yml, which sets it to the pushed tag name).
// Local and CI-only builds that don't pass this keep the default, so a
// binary someone built themselves never claims to be a tagged release it
// isn't.
var Version = "dev"

// Maintainer and RepoURL are constant — they identify the project, not a
// particular build — and are shown in both the CLI (`ngxsetup version`,
// the --help banner) and the web UI's sidebar footer.
const (
	Maintainer = "Vijay Vishwakarma"
	RepoURL    = "https://github.com/sharmavijay86/ngxsetup"
)
