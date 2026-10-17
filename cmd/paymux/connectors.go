package main

// Built-in connectors register themselves in init(). This file is separate
// from main.go so the thin-main line budget never limits the connector list.
import (
	_ "github.com/paymux/paymux/connectors/payu"
	_ "github.com/paymux/paymux/connectors/razorpay"
)
