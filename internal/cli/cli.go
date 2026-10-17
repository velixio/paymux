// Package cli implements the paymux command-line entry point.
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/paymux/paymux"
)

const usage = "usage: paymux <serve|migrate|version>"

// Run executes the CLI and returns the process exit code.
func Run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "paymux %s\n", paymux.Version)
		return 0
	case "serve", "migrate":
		fmt.Fprintf(stderr, "paymux %s: not implemented\n", args[0])
		return 1
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}
