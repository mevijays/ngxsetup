// Command ngxsetup provisions and tunes a WordPress server: nginx, PHP-FPM and
// MariaDB or MySQL, sized for the machine it is running on.
//
// Everything the tool needs is embedded in this one binary — configuration
// templates included — so deployment is a single file copy.
package main

import (
	"os"

	"ngxsetup/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args))
}
