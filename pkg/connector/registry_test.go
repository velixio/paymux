package connector

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func nopFactory(json.RawMessage, Deps) (Connector, error) { return nil, nil }

func TestRegisterLookupList(t *testing.T) {
	ResetRegistryForTest()
	Register("b", nopFactory, Info{Name: "b"})
	Register("a", nopFactory, Info{Name: "a", DisplayName: "A"})
	f, info, ok := Lookup("a")
	if !ok || f == nil || info.DisplayName != "A" {
		t.Fatalf("lookup a: %v %v %v", f, info, ok)
	}
	if _, _, ok := Lookup("zzz"); ok {
		t.Fatal("unexpected hit")
	}
	l := List()
	if len(l) != 2 || l[0].Name != "a" || l[1].Name != "b" {
		t.Fatalf("list not sorted: %+v", l)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	ResetRegistryForTest()
	Register("x", nopFactory, Info{Name: "x"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	Register("x", nopFactory, Info{Name: "x"})
}

func TestRegisterBadArgsPanic(t *testing.T) {
	ResetRegistryForTest()
	for _, c := range []struct {
		name string
		f    Factory
	}{{"", nopFactory}, {"y", nil}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for %q", c.name)
				}
			}()
			Register(c.name, c.f, Info{})
		}()
	}
}

func TestMemKV(t *testing.T) {
	now := time.Unix(1000, 0)
	kv := NewMemKV(func() time.Time { return now })
	ctx := context.Background()
	if _, ok, _ := kv.Get(ctx, "k"); ok {
		t.Fatal("empty kv hit")
	}
	_ = kv.Set(ctx, "k", []byte("v"), time.Minute)
	_ = kv.Set(ctx, "forever", []byte("f"), 0)
	if v, ok, _ := kv.Get(ctx, "k"); !ok || string(v) != "v" {
		t.Fatal("get after set")
	}
	now = now.Add(2 * time.Minute)
	if _, ok, _ := kv.Get(ctx, "k"); ok {
		t.Fatal("ttl not honoured")
	}
	if _, ok, _ := kv.Get(ctx, "forever"); !ok {
		t.Fatal("no-ttl key vanished")
	}
	_ = kv.Delete(ctx, "forever")
	if _, ok, _ := kv.Get(ctx, "forever"); ok {
		t.Fatal("delete failed")
	}
}
