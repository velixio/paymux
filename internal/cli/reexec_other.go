//go:build !unix

package cli

import "errors"

func execSelf(string, []string, []string) error {
	return errors.New("restarting in place is not supported on this platform")
}
