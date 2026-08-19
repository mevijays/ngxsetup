package migrate

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"testing"
)

// TestIsSSHConnectionFailureClassifiesByExitCode locks in the exact
// distinction RunRetry depends on: ssh's own well-documented convention of
// exiting 255 when it could not establish or maintain the connection, vs.
// passing the remote command's own exit status through unchanged for
// everything else.
func TestIsSSHConnectionFailureClassifiesByExitCode(t *testing.T) {
	exit := func(code int) error {
		// The simplest way to obtain a real *exec.ExitError with a chosen
		// exit code without a shell: run this same test binary with `-test.run`
		// matching nothing, which isn't quite it — instead, run `sh -c "exit N"`.
		cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit "+strconv.Itoa(code))
		err := cmd.Run()
		if err == nil && code != 0 {
			t.Fatalf("expected sh -c 'exit %d' to fail", code)
		}
		return err
	}

	if !isSSHConnectionFailure(exit(255)) {
		t.Error("exit code 255 (ssh's own connection-failure code) must be treated as retryable")
	}
	if isSSHConnectionFailure(exit(1)) {
		t.Error("exit code 1 (a plausible remote command failure, e.g. `cat` on a missing file) must NOT be treated as retryable")
	}
	if isSSHConnectionFailure(exit(127)) {
		t.Error("exit code 127 (command not found on the remote end) must NOT be treated as retryable")
	}
	if !isSSHConnectionFailure(errors.New("no exit code at all — e.g. the ssh binary itself could not be started")) {
		t.Error("a non-ExitError failure must default to retryable, preserving the previous always-retry behaviour for unknown failure modes")
	}
}
