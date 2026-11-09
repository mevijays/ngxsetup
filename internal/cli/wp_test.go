package cli

import (
	"context"
	"errors"
	"testing"
)

func TestWPCommandNeedsADomainAndArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"--dry-run"}, {"example.com"}, {"example.com", "--"}} {
		if err := cmdWP(context.Background(), args); err == nil {
			t.Errorf("cmdWP(%q) succeeded, want a usage error", args)
		}
	}
	if err := cmdWP(context.Background(), []string{"--help"}); err != nil {
		t.Errorf("--help = %v, want usage printed without error", err)
	}
}

// A wp-cli exit status must survive to the process exit code, so `ngxsetup
// wp example.com core is-installed` can be scripted like wp-cli itself.
func TestExitCodeErrorCarriesTheStatus(t *testing.T) {
	var err error = exitCodeError(3)
	var code exitCodeError
	if !errors.As(err, &code) || int(code) != 3 {
		t.Errorf("exit status lost: %v", err)
	}
}
