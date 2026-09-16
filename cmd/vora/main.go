// Command vora is the Vora command-line interface.
package main

import (
	"os"

	"github.com/vorash/vora/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
