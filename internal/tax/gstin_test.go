package tax

import "testing"

func TestValidGSTIN(t *testing.T) {
	ok := []string{"27AAPFU0939F1ZV", "29ABCDE1234F1Z" + string(gstinCheck("29ABCDE1234F1Z")), "07ZZZZZ9999Z9Z" + string(gstinCheck("07ZZZZZ9999Z9Z"))}
	for _, g := range ok {
		if err := ValidGSTIN(g); err != nil {
			t.Errorf("ValidGSTIN(%q) = %v, want nil", g, err)
		}
	}
	bad := map[string]string{
		"":                "empty",
		"27AAPFU0939F1Z":  "short",
		"27aapfu0939f1zv": "lowercase",
		"27AAPFU0939F1ZW": "checksum",
		"99AAPFU0939F1ZV": "state",
		"27AAPF10939F1ZV": "pan shape",
		"27AAPFU0939F1YV": "14th char must be Z",
	}
	for g, why := range bad {
		if err := ValidGSTIN(g); err == nil {
			t.Errorf("ValidGSTIN(%q) = nil, want error (%s)", g, why)
		}
	}
}

func TestStateFromGSTIN(t *testing.T) {
	if got := StateFromGSTIN("27AAPFU0939F1ZV"); got != "27" {
		t.Fatalf("got %q", got)
	}
	if n, ok := StateName("27"); !ok || n != "Maharashtra" {
		t.Fatalf("StateName(27) = %q,%v", n, ok)
	}
	if _, ok := StateName("25"); ok {
		t.Fatal("25 is retired (merged into 26)")
	}
	if n, _ := StateName("26"); n != "Dadra and Nagar Haveli and Daman and Diu" {
		t.Fatalf("26 = %q", n)
	}
}
