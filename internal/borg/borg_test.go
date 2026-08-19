package borg

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ngxsetup/internal/system"
)

// These tests drive a real, local borg binary against a throwaway
// filesystem repository — the same trade-off internal/db's tests would make
// if a real MySQL were as cheap to spin up as a local directory is here.
// Skipped automatically wherever borg isn't installed (CI, a machine
// without it) rather than failing.
func requireBorg(t *testing.T) {
	t.Helper()
	if !(system.Runner{}).Look("borg") {
		t.Skip("borg is not installed; skipping (see internal/borg package doc)")
	}
}

func newTestClient(t *testing.T) Client {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	passFile := filepath.Join(dir, "passphrase")
	if err := SetPassphrase(passFile, "correct horse battery staple test only"); err != nil {
		t.Fatal(err)
	}
	return Client{Runner: system.Runner{}, Repo: repo, PassphraseFile: passFile}
}

func TestSetPassphraseRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "passphrase")
	if err := SetPassphrase(path, "hello world"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "hello world" {
		t.Errorf("decoded = %q, want %q", decoded, "hello world")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("passphrase file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestClientNotReachableBeforeInit(t *testing.T) {
	requireBorg(t)
	c := newTestClient(t)
	if c.Reachable(context.Background()) {
		t.Error("a repository that was never initialised reported as reachable")
	}
}

func TestInitIsIdempotent(t *testing.T) {
	requireBorg(t)
	c := newTestClient(t)
	ctx := context.Background()

	if err := c.Init(ctx, "repokey-blake2"); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	if !c.Reachable(ctx) {
		t.Fatal("repository not reachable immediately after Init")
	}
	// A second Init against the same, already-initialised repository must
	// not error — this is what lets setup be re-run safely.
	if err := c.Init(ctx, "repokey-blake2"); err != nil {
		t.Fatalf("second Init on an already-initialised repo: %v", err)
	}
}

func TestCreateListAndExtractArchive(t *testing.T) {
	requireBorg(t)
	c := newTestClient(t)
	ctx := context.Background()
	if err := c.Init(ctx, "repokey-blake2"); err != nil {
		t.Fatal(err)
	}

	// A small "site" to back up: a file tree plus a fake SQL dump, exactly
	// the shape a real site backup uses (document root + a staged dump).
	//
	// t.TempDir() resolved through its real path: on macOS /var is itself a
	// symlink to /private/var, and borg's own archive-safety check refuses
	// to extract beneath a symlinked parent ("a parent directory is a
	// symlink (malicious or corrupted archive)") — confirmed live, this
	// failed the in-place restore below until resolved here. Ubuntu, the
	// actual deployment target, has no such symlink in this path, so this
	// is purely a macOS test-environment quirk, not a production concern —
	// resolving it up front is what lets this test run correctly on both.
	srcDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	siteDir := filepath.Join(srcDir, "site")
	if err := os.MkdirAll(siteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "index.php"), []byte("<?php echo 'hi';"), 0o644); err != nil {
		t.Fatal(err)
	}
	dumpPath := filepath.Join(srcDir, "site.sql")
	if err := os.WriteFile(dumpPath, []byte("CREATE TABLE t (id INT);\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.CreateArchive(ctx, "test-archive", "zstd", []string{siteDir, dumpPath}); err != nil {
		t.Fatalf("CreateArchive: %v", err)
	}

	archives, err := c.ListArchives(ctx)
	if err != nil {
		t.Fatalf("ListArchives: %v", err)
	}
	if len(archives) != 1 || archives[0].Name != "test-archive" {
		t.Fatalf("archives = %+v, want exactly one named test-archive", archives)
	}
	if archives[0].Time == "" {
		t.Error("archive has no timestamp")
	}

	// Restore the SQL dump into a fresh directory and check its content —
	// the extract-somewhere-else path a "restore just the database" flow
	// uses.
	restoreDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ExtractPath(ctx, "test-archive", dumpPath, restoreDir); err != nil {
		t.Fatalf("ExtractPath (dump): %v", err)
	}
	restored, err := os.ReadFile(filepath.Join(restoreDir, strings.TrimPrefix(dumpPath, "/")))
	if err != nil {
		t.Fatalf("reading extracted dump: %v", err)
	}
	if string(restored) != "CREATE TABLE t (id INT);\n" {
		t.Errorf("extracted dump content = %q", restored)
	}

	// Restore the site directory in place (dir="/") and check it landed
	// back at its original absolute path — the "restore files" flow.
	if err := c.ExtractPath(ctx, "test-archive", siteDir, "/"); err != nil {
		t.Fatalf("ExtractPath (site dir, in place): %v", err)
	}
	inPlace, err := os.ReadFile(filepath.Join(siteDir, "index.php"))
	if err != nil {
		t.Fatalf("reading in-place-restored file: %v", err)
	}
	if string(inPlace) != "<?php echo 'hi';" {
		t.Errorf("in-place restored content = %q", inPlace)
	}
}

func TestDeleteArchiveRemovesOnlyThatArchive(t *testing.T) {
	requireBorg(t)
	c := newTestClient(t)
	ctx := context.Background()
	if err := c.Init(ctx, "repokey-blake2"); err != nil {
		t.Fatal(err)
	}

	srcDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.CreateArchive(ctx, "keep-me", "zstd", []string{srcDir}); err != nil {
		t.Fatalf("CreateArchive(keep-me): %v", err)
	}
	if err := c.CreateArchive(ctx, "delete-me", "zstd", []string{srcDir}); err != nil {
		t.Fatalf("CreateArchive(delete-me): %v", err)
	}

	if err := c.DeleteArchive(ctx, "delete-me"); err != nil {
		t.Fatalf("DeleteArchive: %v", err)
	}

	archives, err := c.ListArchives(ctx)
	if err != nil {
		t.Fatalf("ListArchives: %v", err)
	}
	if len(archives) != 1 || archives[0].Name != "keep-me" {
		t.Fatalf("archives after delete = %+v, want exactly one named keep-me", archives)
	}
}

func TestDeleteArchiveRejectsEmptyName(t *testing.T) {
	c := newTestClient(t)
	if err := c.DeleteArchive(context.Background(), ""); err == nil {
		t.Error("DeleteArchive accepted an empty archive name")
	}
}

func TestStatsReportsRepoAndPerArchiveSizes(t *testing.T) {
	requireBorg(t)
	c := newTestClient(t)
	ctx := context.Background()
	if err := c.Init(ctx, "repokey-blake2"); err != nil {
		t.Fatal(err)
	}

	srcDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.CreateArchive(ctx, "first", "zstd", []string{srcDir}); err != nil {
		t.Fatalf("CreateArchive(first): %v", err)
	}
	// A second archive of the exact same, unchanged content: every chunk is
	// already known, so this archive's own DeduplicatedSize (its marginal,
	// unique contribution) must be zero even though its OriginalSize is not.
	if err := c.CreateArchive(ctx, "second", "zstd", []string{srcDir}); err != nil {
		t.Fatalf("CreateArchive(second): %v", err)
	}

	repo, archives, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if repo.UniqueChunks == 0 || repo.TotalChunks == 0 {
		t.Errorf("repo chunk counts = %+v, want non-zero", repo)
	}
	if repo.UniqueChunks > repo.TotalChunks {
		t.Errorf("unique chunks (%d) exceeds total chunks (%d)", repo.UniqueChunks, repo.TotalChunks)
	}
	if repo.UniqueCompressedSize <= 0 {
		t.Errorf("UniqueCompressedSize = %d, want > 0 (the repository has real archived data)", repo.UniqueCompressedSize)
	}
	if ratio := repo.DedupRatio(); ratio < 0 || ratio > 1 {
		t.Errorf("DedupRatio = %v, want between 0 and 1", ratio)
	}

	if len(archives) != 2 {
		t.Fatalf("archives = %+v, want exactly 2", archives)
	}
	byName := map[string]ArchiveDetail{}
	for _, a := range archives {
		byName[a.Name] = a
	}
	first, ok := byName["first"]
	if !ok {
		t.Fatal("archive 'first' missing from Stats output")
	}
	if first.OriginalSize <= 0 || first.NFiles != 1 {
		t.Errorf("first archive stats = %+v, want OriginalSize > 0 and NFiles 1", first)
	}
	second, ok := byName["second"]
	if !ok {
		t.Fatal("archive 'second' missing from Stats output")
	}
	if second.DeduplicatedSize != 0 {
		t.Errorf("second archive's DeduplicatedSize = %d, want 0 (identical content to 'first', nothing new)", second.DeduplicatedSize)
	}
}

func TestStatsOnEmptyRepository(t *testing.T) {
	requireBorg(t)
	c := newTestClient(t)
	ctx := context.Background()
	if err := c.Init(ctx, "repokey-blake2"); err != nil {
		t.Fatal(err)
	}
	repo, archives, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats on a freshly initialised, empty repository: %v", err)
	}
	if len(archives) != 0 {
		t.Errorf("archives = %+v, want none", archives)
	}
	if repo.DedupRatio() != 0 {
		t.Errorf("DedupRatio on an empty repository = %v, want 0", repo.DedupRatio())
	}
}

func TestPruneWithNoRetentionConfiguredIsANoOp(t *testing.T) {
	requireBorg(t)
	c := newTestClient(t)
	ctx := context.Background()
	if err := c.Init(ctx, "repokey-blake2"); err != nil {
		t.Fatal(err)
	}
	// No archives, no retention rules — must succeed trivially rather than
	// erroring on an empty repository or an all-zero policy.
	if err := c.Prune(ctx, Retention{}); err != nil {
		t.Errorf("Prune with an empty retention policy returned an error: %v", err)
	}
}

func TestShellQuoteHandlesEmbeddedSingleQuotes(t *testing.T) {
	cases := map[string]string{
		"plain": `'plain'`,
		"it's":  `'it'\''s'`,
		"":      `''`,
		"a'b'c": `'a'\''b'\''c'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDirOf(t *testing.T) {
	cases := map[string]string{
		"/etc/ngxsetup/borg-passphrase": "/etc/ngxsetup",
		"/passphrase":                   "/",
		"relative":                      "/",
	}
	for in, want := range cases {
		if got := dirOf(in); got != want {
			t.Errorf("dirOf(%q) = %q, want %q", in, got, want)
		}
	}
}
