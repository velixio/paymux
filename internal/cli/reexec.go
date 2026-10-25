package cli

import (
	"fmt"
	"io"
	"os"
)

// reexec replaces the process with exe (replaced in tests).
var reexec = execSelf

// finishServe ends "paymux serve": after a stop that an installed update asked
// for, the process starts the new binary in place (the same arguments and
// environment, so the same configuration); otherwise it just exits.
func finishServe(restart bool, exe string, stderr io.Writer) int {
	if !restart {
		return 0
	}
	if err := reexec(exe, os.Args, os.Environ()); err != nil {
		fmt.Fprintf(stderr, "paymux serve: the update is installed but paymux could not restart itself: %v; start it again\n", err)
		return 1
	}
	return 0
}
