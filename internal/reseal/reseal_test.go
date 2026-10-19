package reseal_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/reseal"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	keyA = bytes.Repeat([]byte{'a'}, 32)
	keyB = bytes.Repeat([]byte{'b'}, 32)
	keyC = bytes.Repeat([]byte{'c'}, 32)
)

func mustBox(t *testing.T, active []byte, older ...[]byte) *secrets.Box {
	t.Helper()
	b, err := secrets.NewBox(active, older...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func seal(t *testing.T, b *secrets.Box, plain string, aad []byte) []byte {
	t.Helper()
	ct, err := b.Seal([]byte(plain), aad)
	if err != nil {
		t.Fatal(err)
	}
	return ct
}

// seedSealed stores one sealed value of every kind, all under box.
func seedSealed(t *testing.T, db *store.DB, box *secrets.Box) {
	t.Helper()
	err := db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Paymux", t0); err != nil {
			return err
		}
		if err := store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "Shop", Slug: "shop", CreatedAt: t0}); err != nil {
			return err
		}
		if err := store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_1", OrgID: store.DefaultOrgID, Mode: "live", Connector: "x",
			Name: "n", Status: "active", ConfigEnc: seal(t, box, `{"key":"v"}`, secrets.AADProviderConfig("pa_1")), CreatedAt: t0}); err != nil {
			return err
		}
		if err := db.InsertEndpoint(ctx, tx, store.EndpointRow{ID: "we_1", OrgID: store.DefaultOrgID, AppID: "app_1", URL: "https://example.com/h",
			SigningSecretEnc: seal(t, box, "whsec_x", secrets.AADEndpointSecret("we_1")), Enabled: true, Mode: "live", CreatedAt: t0, UpdatedAt: t0}); err != nil {
			return err
		}
		if err := store.InsertPayout(ctx, tx, store.Payout{ID: "po_1", OrgID: store.DefaultOrgID, Mode: "live", AppID: "app_1", ProviderAccountID: "pa_1",
			ProviderRef: "r1", Amount: 100, Currency: "INR", Status: "pending", BeneficiaryEnc: seal(t, box, `{"acct":"1"}`, secrets.AADBeneficiary("po_1")),
			BeneficiaryLabel: "x", Version: 1, CreatedAt: t0, UpdatedAt: t0}); err != nil {
			return err
		}
		if err := store.InsertTaxEntity(ctx, tx, store.TaxEntity{ID: "te_1", OrgID: store.DefaultOrgID, LegalName: "L", GSTIN: "27AAPFU0939F1ZV", PAN: "AAPFU0939F",
			Address: "a", StateCode: "27", InvoicePrefix: "INV", CreditNotePrefix: "CN", CreatedAt: t0, UpdatedAt: t0}); err != nil {
			return err
		}
		if err := store.SetTaxEntityDSC(ctx, tx, "te_1", store.EntityDSC{Enc: seal(t, box, "p12", secrets.AADTaxEntityDSC("te_1")),
			Subject: "s", Fingerprint: "f", NotAfter: t0.Add(time.Hour), UploadedAt: t0}, t0); err != nil {
			return err
		}
		if err := store.ConnectorKVSet(ctx, tx, "pa_1", "token", seal(t, box, "tok", secrets.AADConnectorKV("pa_1", "token")), time.Time{}); err != nil {
			return err
		}
		if _, err := store.ReserveIdempotencyKey(ctx, tx, "app_1", "live", "k1", "own", "h", t0, 24*time.Hour, time.Minute); err != nil {
			return err
		}
		return store.CompleteIdempotencyKey(ctx, tx, "app_1", "live", "k1", "own", 201, seal(t, box, `{"id":"pay_1"}`, secrets.AADIdempotency("app_1", "live", "k1")))
	})
	if err != nil {
		t.Fatal(err)
	}
	u := store.UserRow{ID: "usr_1", Email: "a@example.com", PasswordHash: "h", TOTPLastStep: 5, Role: "admin", CreatedAt: t0,
		TOTPSecretEnc: seal(t, box, "JBSWY3DP", secrets.AADTOTP("usr_1"))}
	if err := db.InsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
}

const kinds = 7 // provider config, endpoint secret, payout beneficiary, DSC, connector KV, idempotent response, TOTP secret

func items(t *testing.T, db *store.DB) []store.SealedItem {
	t.Helper()
	got, err := db.SealedItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func opensWith(t *testing.T, box *secrets.Box, db *store.DB) int {
	t.Helper()
	n := 0
	for _, it := range items(t, db) {
		if _, err := box.Open(it.Enc, it.AAD); err != nil {
			t.Errorf("%s %s: %v", it.Kind, it.ID, err)
		}
		n++
	}
	return n
}

// The whole rotation: seal under A, switch to B keeping A as an old key,
// reseal, then drop A.
func TestRotatingTheMasterKeyKeepsEverySealedValueReadable(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedSealed(t, db, mustBox(t, keyA))
		if n := len(items(t, db)); n != kinds {
			t.Fatalf("seeded %d sealed values, want %d", n, kinds)
		}
		newBox := mustBox(t, keyB, keyA)
		// Before resealing, B alone cannot read anything; with A as an old key it reads all.
		if _, err := mustBox(t, keyB).Open(items(t, db)[0].Enc, items(t, db)[0].AAD); err == nil {
			t.Fatal("B alone read a value sealed by A")
		}
		opensWith(t, newBox, db)

		res, err := reseal.Run(ctx, db, newBox, false)
		if err != nil || res.Resealed != kinds || res.Current != 0 {
			t.Fatalf("reseal: %+v %v", res, err)
		}
		for _, it := range items(t, db) {
			if id, _ := secrets.KeyIDOf(it.Enc); id != secrets.KeyID(keyB) {
				t.Errorf("%s %s is still sealed under key %s", it.Kind, it.ID, id)
			}
		}
		// Now the old key can go.
		if n := opensWith(t, mustBox(t, keyB), db); n != kinds {
			t.Errorf("%d values readable with B alone", n)
		}
		// Idempotent: nothing left to do, and the bytes do not change.
		before := items(t, db)
		res, err = reseal.Run(ctx, db, newBox, false)
		if err != nil || res.Resealed != 0 || res.Current != kinds {
			t.Fatalf("second reseal: %+v %v", res, err)
		}
		for i, it := range items(t, db) {
			if !bytes.Equal(it.Enc, before[i].Enc) {
				t.Errorf("%s %s was rewritten by a no-op run", it.Kind, it.ID)
			}
		}
	})
}

// A value the given keys cannot open stops the whole run, and nothing is written.
func TestResealRefusesWhenAnyValueCannotBeOpened(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedSealed(t, db, mustBox(t, keyA))
		// One value was sealed under a key nobody has any more.
		if err := db.Write(ctx, func(tx store.Tx) error {
			return store.ConnectorKVSet(ctx, tx, "pa_1", "token", seal(t, mustBox(t, keyC), "tok", secrets.AADConnectorKV("pa_1", "token")), time.Time{})
		}); err != nil {
			t.Fatal(err)
		}
		before := items(t, db)
		_, err := reseal.Run(ctx, db, mustBox(t, keyB, keyA), false)
		if err == nil || !strings.Contains(err.Error(), "connector_kv") || !strings.Contains(err.Error(), "pa_1") {
			t.Fatalf("err = %v", err)
		}
		for _, secret := range []string{"tok", "whsec_x", "JBSWY3DP"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error leaks %q", secret)
			}
		}
		for i, it := range items(t, db) {
			if !bytes.Equal(it.Enc, before[i].Enc) {
				t.Errorf("%s %s changed although the run was refused", it.Kind, it.ID)
			}
		}
		// A value sealed for another row (the wrong additional data) is refused as well.
		if err := db.Write(ctx, func(tx store.Tx) error {
			return store.ConnectorKVSet(ctx, tx, "pa_1", "token", seal(t, mustBox(t, keyA), "tok", secrets.AADConnectorKV("pa_1", "other")), time.Time{})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := reseal.Run(ctx, db, mustBox(t, keyB, keyA), false); err == nil {
			t.Error("a value bound to another row was resealed")
		}
	})
}

func TestResealDryRunChangesNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedSealed(t, db, mustBox(t, keyA))
		before := items(t, db)
		res, err := reseal.Run(ctx, db, mustBox(t, keyB, keyA), true)
		if err != nil || res.Resealed != kinds {
			t.Fatalf("dry run: %+v %v", res, err)
		}
		for i, it := range items(t, db) {
			if !bytes.Equal(it.Enc, before[i].Enc) {
				t.Errorf("%s %s changed in a dry run", it.Kind, it.ID)
			}
		}
	})
}

// A value that changed between the read and the write (the hub is running and
// sealed a new one) is not overwritten with a stale one.
func TestResealDoesNotOverwriteAValueThatChangedMeanwhile(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedSealed(t, db, mustBox(t, keyA))
		stale := items(t, db)
		fresh := seal(t, mustBox(t, keyA), "newer", secrets.AADConnectorKV("pa_1", "token"))
		if err := db.Write(ctx, func(tx store.Tx) error { return store.ConnectorKVSet(ctx, tx, "pa_1", "token", fresh, time.Time{}) }); err != nil {
			t.Fatal(err)
		}
		changes := []store.SealedChange{{Item: stale[0], NewEnc: []byte("x")}}
		for _, it := range stale {
			if it.Kind == "connector_kv" {
				changes = []store.SealedChange{{Item: it, NewEnc: []byte("overwritten")}}
			}
		}
		if err := db.ReplaceSealed(ctx, changes); err == nil {
			t.Fatal("a stale replacement was applied")
		}
		for _, it := range items(t, db) {
			if it.Kind == "connector_kv" && !bytes.Equal(it.Enc, fresh) {
				t.Error("the newer value was overwritten")
			}
		}
	})
}

var _ = migrations.FS
