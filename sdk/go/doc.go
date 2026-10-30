// Package paymux is the official Go client for the Paymux merchant API.
//
// Amounts are integer minor units. Every method accepts a context, and every
// mutation requires an explicit idempotency key. The SDK never redirects an
// end user: create operations return a checkout URL for the application to use.
package paymux
