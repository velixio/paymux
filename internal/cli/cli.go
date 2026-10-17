// Package cli implements the paymux command-line entry point.
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/paymux/paymux"
	"github.com/paymux/paymux/internal/config"
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
		if _, err := envWithDotEnv(lookupEnv); err != nil {
			fmt.Fprintf(stderr, "paymux %s: %v\n", args[0], err)
			return 1
		}
		fmt.Fprintf(stderr, "paymux %s: not implemented\n", args[0])
		return 1
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

// envWithDotEnv layers the .env file (PAYMUX_ENV_FILE, default ".env") under
// the real environment.
func envWithDotEnv(lookupEnv func(string) (string, bool)) (func(string) (string, bool), error) {
	path := ".env"
	if v, ok := lookupEnv("PAYMUX_ENV_FILE"); ok && v != "" {
		path = v
	}
	file, err := config.LoadDotEnv(path)
	if err != nil {
		return nil, err
	}
	return config.WithDotEnv(lookupEnv, file), nil
}
