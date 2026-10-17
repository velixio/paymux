// Package all links every built-in connector into the binary.
package all

import (
	_ "github.com/paymux/paymux/connectors/payu"
	_ "github.com/paymux/paymux/connectors/razorpay"
)
