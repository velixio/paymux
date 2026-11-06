package redact

import "testing"

// A try's method details hold the name on the card: it must not reach logs.
func TestCardNameRedacted(t *testing.T) {
	r := New()
	got := r.Body("application/json", []byte(`{"method_details":{"card":{"last4":"4242","name":"S CUSTOMER"}},"name_on_card":"A PERSON"}`))
	notContains(t, "json", got, "S CUSTOMER", "A PERSON")
}
