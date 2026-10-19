package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func keyB64(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{b}), 32)))
}

func TestOldMasterKeysFromTheEnvironment(t *testing.T) {
	c, err := Load(base("PAYMUX_MASTER_KEY_OLD", keyB64('o')+" , "+keyB64('p')))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.OldMasterKeys) != 2 || string(c.OldMasterKeys[0]) != strings.Repeat("o", 32) || string(c.OldMasterKeys[1]) != strings.Repeat("p", 32) {
		t.Fatalf("old keys %q", c.OldMasterKeys)
	}
	if c, err = Load(base()); err != nil || len(c.OldMasterKeys) != 0 {
		t.Errorf("none given: %v %v", c.OldMasterKeys, err)
	}
}

func TestOldMasterKeysFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.keys")
	if err := os.WriteFile(path, []byte(keyB64('o')+"\n"+keyB64('p')+",\n\n"+keyB64('q')+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(base("PAYMUX_MASTER_KEY_OLD_FILE", path, "PAYMUX_MASTER_KEY_OLD", keyB64('r')))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.OldMasterKeys) != 4 {
		t.Fatalf("old keys: %d", len(c.OldMasterKeys))
	}
	wantErr(t, mustErr(Load(base("PAYMUX_MASTER_KEY_OLD_FILE", path+".missing"))), "PAYMUX_MASTER_KEY_OLD_FILE")
}

func TestOldMasterKeysAreValidated(t *testing.T) {
	wantErr(t, mustErr(Load(base("PAYMUX_MASTER_KEY_OLD", "not base64!"))), "PAYMUX_MASTER_KEY_OLD")
	wantErr(t, mustErr(Load(base("PAYMUX_MASTER_KEY_OLD", base64.StdEncoding.EncodeToString([]byte("short"))))), "32 bytes")
	// The active key listed among the old ones is harmless and not duplicated.
	c, err := Load(base("PAYMUX_MASTER_KEY_OLD", goodKey+","+keyB64('o')))
	if err != nil || len(c.OldMasterKeys) != 1 {
		t.Errorf("active key among the old ones: %d keys, %v", len(c.OldMasterKeys), err)
	}
}

func mustErr(_ Config, err error) error { return err }
