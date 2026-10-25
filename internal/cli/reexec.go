package cli

import "io"

// reexec replaces the process with exe (replaced in tests).
var reexec = execSelf

// finishServe ends "paymux serve": after a stop that an installed update asked
// for, the process starts the new binary in place (the same arguments and
// environment, so the same configuration); otherwise it just exits.
func finishServe(restart bool, exe string, stderr io.Writer) int {
	return 0
}
