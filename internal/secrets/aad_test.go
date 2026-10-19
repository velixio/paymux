package secrets

import (
	"bytes"
	"errors"
	"testing"
)

func TestKeyIDOfAndActiveKeyID(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	old, _ := NewBox(a)
	box, _ := NewBox(b, a)
	ct, _ := old.Seal([]byte("x"), []byte("aad"))
	if id, ok := KeyIDOf(ct); !ok || id != KeyID(a) {
		t.Errorf("KeyIDOf = %q %v", id, ok)
	}
	if box.ActiveKeyID() != KeyID(b) {
		t.Errorf("active %q", box.ActiveKeyID())
	}
	if _, ok := KeyIDOf([]byte{9, 9}); ok {
		t.Error("garbage has a key id")
	}
	// The new box reads what the old key sealed, and seals under its own.
	pt, err := box.Open(ct, []byte("aad"))
	if err != nil || string(pt) != "x" {
		t.Fatalf("open with the old key: %q %v", pt, err)
	}
	ct2, _ := box.Seal(pt, []byte("aad"))
	if id, _ := KeyIDOf(ct2); id != KeyID(b) {
		t.Errorf("resealed under %q", id)
	}
	// Without the old key the old ciphertext is unreadable, and says so.
	only, _ := NewBox(b)
	if _, err := only.Open(ct, []byte("aad")); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("without the old key: %v", err)
	}
}

// The additional data binds a sealed value to its row; these are the strings
// every package seals with and the key rotation reads with.
func TestAdditionalDataIsStable(t *testing.T) {
	for got, want := range map[string]string{
		string(AADEndpointSecret("we_1")):            "webhook_endpoints.signing_secret:we_1",
		string(AADProviderConfig("pa_1")):            "provider_accounts.config_enc:pa_1",
		string(AADTOTP("usr_1")):                     "users.totp_secret:usr_1",
		string(AADBeneficiary("po_1")):               "payouts.beneficiary:po_1",
		string(AADTaxEntityDSC("te_1")):              "tax_entities.dsc_enc:te_1",
		string(AADConnectorKV("pa_1", "token")):      "connector_kv:pa_1:token",
		string(AADIdempotency("app_1", "live", "k")): "app_1/live/k",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
