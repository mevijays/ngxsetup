package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"ngxsetup/internal/provision"
)

// configRows and setConfigKey live in package provision now
// (provision.ConfigRows / provision.SetConfigKey) so the web UI's settings
// page reads and writes through the exact same table instead of a second,
// separately maintained copy. These are kept as thin local names so the rest
// of this file's callers did not need to change.
func configRows(c *provision.Ctx) [][2]string { return provision.ConfigRows(c) }
func setConfigKey(c *provision.Ctx, key, val string) error {
	return provision.SetConfigKey(c, key, val)
}

// readSecret prompts for a value without echoing it to the terminal.
//
// Passwords must not be passed as command-line arguments — argv is world
// readable through /proc — and they should not be left in shell history
// either, so this is the only way the tool accepts one.
func readSecret(prompt string) (string, error) {
	fmt.Print(prompt)
	restore := disableEcho()
	defer restore()

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Println()
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(line)
	if secret == "" {
		return "", fmt.Errorf("no password entered")
	}
	return secret, nil
}

// disableEcho turns off terminal echo through stty, returning a function that
// restores it. stty is used rather than a termios binding to keep the binary
// free of external dependencies; if it is unavailable the prompt still works,
// it just echoes.
func disableEcho() func() {
	if _, err := os.Stdin.Stat(); err != nil {
		return func() {}
	}
	cmd := exec.Command("stty", "-echo")
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return func() {}
	}
	return func() {
		restore := exec.Command("stty", "echo")
		restore.Stdin = os.Stdin
		_ = restore.Run()
	}
}
