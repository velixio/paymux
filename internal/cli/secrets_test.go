package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/config"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
)

func lookupWith(base func(string) (string, bool), kv ...string) func(string) (string, bool) {
	over := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		over[kv[i]] = kv[i+1]
	}
	return func(k string) (string, bool) {
		if v, ok := over[k]; ok {
			return v, v != ""
		}
		return base(k)
	}
}

func b64key(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

// "paymux secrets reseal" after a master key change: the new key is active, the
// old one is listed, and every sealed value is rewritten under the new one.
func TestSecretsResealRotatesTheMasterKey(t *testing.T) {
	dir := t.TempDir()
	oldEnv := lookupWith(migrateEnv(dir), "PAYMUX_MASTER_KEY", b64key('a'))
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"migrate"}, oldEnv, &out, &errb); code != 0 {
		t.Fatalf("migrate: %s", errb.String())
	}
	oldBox, _ := secrets.NewBox(bytes.Repeat([]byte{'a'}, 32))
	db := openDB(t, oldEnv)
	enc, _ := oldBox.Seal([]byte("JBSWY3DP"), secrets.AADTOTP("usr_1"))
	if err := db.InsertUser(context.Background(), store.UserRow{ID: "usr_1", Email: "a@example.com", PasswordHash: "h", TOTPLastStep: 3, Role: "admin",
		CreatedAt: time.Now(), TOTPSecretEnc: enc}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	newEnv := lookupWith(oldEnv, "PAYMUX_MASTER_KEY", b64key('b'), "PAYMUX_MASTER_KEY_OLD", b64key('a'))
	out.Reset()
	errb.Reset()
	if code := Run(context.Background(), []string{"secrets", "reseal"}, newEnv, &out, &errb); code != 0 {
		t.Fatalf("reseal: code %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "resealed 1 ") {
		t.Errorf("stdout=%q", out.String())
	}
	// The new key alone reads it.
	newBox, _ := secrets.NewBox(bytes.Repeat([]byte{'b'}, 32))
	db = openDB(t, newEnv)
	u, _ := db.UserByID(context.Background(), "usr_1")
	if pt, err := newBox.Open(u.TOTPSecretEnc, secrets.AADTOTP("usr_1")); err != nil || string(pt) != "JBSWY3DP" {
		t.Errorf("after reseal: %q %v", pt, err)
	}
	db.Close()
	// Again: nothing to do.
	out.Reset()
	if code := Run(context.Background(), []string{"secrets", "reseal"}, newEnv, &out, &errb); code != 0 || !strings.Contains(out.String(), "resealed 0 ") {
		t.Errorf("second run: code %d stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	// Without the old key a value sealed under it would be unreadable: refuse.
	wrong := lookupWith(oldEnv, "PAYMUX_MASTER_KEY", b64key('c'), "PAYMUX_MASTER_KEY_OLD", b64key('d'))
	db = openDB(t, oldEnv)
	enc2, _ := oldBox.Seal([]byte("x"), secrets.AADTOTP("usr_2"))
	if err := db.InsertUser(context.Background(), store.UserRow{ID: "usr_2", Email: "b@example.com", PasswordHash: "h", TOTPLastStep: 3, Role: "viewer",
		CreatedAt: time.Now(), TOTPSecretEnc: enc2}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	errb.Reset()
	if code := Run(context.Background(), []string{"secrets", "reseal"}, wrong, &out, &errb); code == 0 || !strings.Contains(errb.String(), "usr_2") {
		t.Errorf("refusal: code %d stderr=%q", code, errb.String())
	}
	_ = config.Config{}
}

func TestSecretsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"secrets"}, migrateEnv(t.TempDir()), &out, &errb); code != 2 || !strings.Contains(errb.String(), "reseal") {
		t.Errorf("code %d stderr=%q", code, errb.String())
	}
}
