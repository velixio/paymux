package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func dscFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "invoice", "pades", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTaxEntityPageManagesTheDSC(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, ent := e.taxApp("alpha", supplierGSTIN)
		page := "/dashboard/tax-entities/" + ent.ID
		admin, viewer := e.roleClient("admin"), e.roleClient("viewer")

		// With no certificate: a standing warning that recommends one, and the upload form.
		body := e.pageOK(admin, page)
		has(t, body, "digital signature", "recommend", `action="`+page+`/dsc"`, `name="dsc"`, `name="dsc_password"`)
		lacks(t, body, `action="`+page+`/dsc/remove"`)
		lacks(t, e.pageOK(viewer, page), `action="`+page+`/dsc"`)
		tok := csrfOf(t, body)

		// A viewer cannot upload.
		if r, _ := e.postMultipart(viewer, page+"/dsc", map[string]string{"csrf": csrfOf(t, e.pageOK(viewer, "/dashboard/tax-entities")), "dsc_password": "test-dsc-pass"},
			"dsc", "dsc.p12", dscFile(t, "rsa.p12")); r.StatusCode != 403 {
			t.Errorf("viewer upload: %d", r.StatusCode)
		}
		// A wrong password is explained and nothing is stored.
		r, b := e.postMultipart(admin, page+"/dsc", map[string]string{"csrf": tok, "dsc_password": "wrong"}, "dsc", "dsc.p12", dscFile(t, "rsa.p12"))
		if r.StatusCode != 400 || !strings.Contains(b, "password") {
			t.Errorf("wrong password: %d %s", r.StatusCode, b)
		}
		// No file.
		if r, _ := e.postMultipart(admin, page+"/dsc", map[string]string{"csrf": tok, "dsc_password": "x"}, "", "", nil); r.StatusCode != 400 {
			t.Errorf("no file: %d", r.StatusCode)
		}
		// The right one.
		r, b = e.postMultipart(admin, page+"/dsc", map[string]string{"csrf": tok, "dsc_password": "test-dsc-pass"}, "dsc", "dsc.p12", dscFile(t, "rsa.p12"))
		if r.StatusCode != 303 || r.Header.Get("Location") != page {
			t.Fatalf("upload: %d %s", r.StatusCode, b)
		}
		body = e.pageOK(admin, page)
		has(t, body, "Test Signatory", "2036-01-01", `action="`+page+`/dsc/remove"`, "Replace")
		lacks(t, body, "recommend a digital signature")
		// The password and the file never come back to the page.
		lacks(t, body, "test-dsc-pass")
		has(t, e.pageOK(viewer, page), "Test Signatory")
		lacks(t, e.pageOK(viewer, page), `action="`+page+`/dsc/remove"`)

		// Remove.
		if r, _ := e.post(viewer, page+"/dsc/remove", map[string][]string{"csrf": {tok}}); r.StatusCode != 403 {
			t.Errorf("viewer remove: %d", r.StatusCode)
		}
		if r, _ := e.post(admin, page+"/dsc/remove", map[string][]string{"csrf": {tok}}); r.StatusCode != 303 {
			t.Errorf("remove: %d", r.StatusCode)
		}
		has(t, e.pageOK(admin, page), "recommend")
		// Another org's entity is not reachable.
		if r, _ := e.post(admin, "/dashboard/tax-entities/te_nope/dsc/remove", map[string][]string{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("unknown entity: %d", r.StatusCode)
		}
	})
}
