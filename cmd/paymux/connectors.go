package main

// Built-in connectors register themselves in init(). This file is separate
// from main.go so the thin-main line budget never limits the connector list.
import (
	_ "github.com/velixio/paymux/connectors/cashfree"
	_ "github.com/velixio/paymux/connectors/payu"
	_ "github.com/velixio/paymux/connectors/razorpay"
	_ "github.com/velixio/paymux/connectors/remote"
	_ "github.com/velixio/paymux/connectors/simulator"
	_ "github.com/velixio/paymux/connectors/stripe"
	_ "github.com/velixio/paymux/connectors/whop"
	_ "github.com/velixio/paymux/connectors/zoho"
)
