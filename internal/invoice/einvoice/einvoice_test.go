package einvoice_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/invoice/einvoice"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var ist = time.FixedZone("IST", 5*3600+1800)

var seller = einvoice.Party{GSTIN: "27AAPFU0939F1ZV", LegalName: "Voice Studio Pvt Ltd", TradeName: "Voicestudio", StateCode: "27",
	Address: "1 Marine Drive, Churchgate, Mumbai, Maharashtra 400020", Email: "help@example.com", Phone: "+91 98200 12345"}

func intraB2B() einvoice.Source {
	return einvoice.Source{Number: "INV/2627/000001", IssuedAt: time.Date(2026, 10, 17, 11, 30, 0, 0, ist), SupplyType: einvoice.B2B,
		PlaceOfSupply: "27", Seller: seller,
		Buyer: einvoice.Party{GSTIN: "27AABCU9603R1ZX", LegalName: "Acme Traders LLP", Address: "22 Linking Road, Bandra West, Mumbai 400050", StateCode: "27"},
		Lines: []einvoice.Line{{Description: "Pro plan, annual", SAC: "998315", Qty: 1, Taxable: 100000, CGSTRateBP: 900, SGSTRateBP: 900, CGST: 9000, SGST: 9000}}}
}

func interB2B() einvoice.Source {
	s := intraB2B()
	s.Number, s.PlaceOfSupply = "INV/2627/000002", "29"
	s.Buyer = einvoice.Party{GSTIN: "29AAGCB7383J1Z4", LegalName: "Bangalore Bytes Pvt Ltd", StateCode: "29",
		Line1: "5 Residency Road", Line2: "Ashok Nagar", City: "Bengaluru", PostalCode: "560025"}
	s.Lines = []einvoice.Line{{Description: "Pro plan, annual", SAC: "998315", Qty: 1, Taxable: 100000, IGSTRateBP: 1800, IGST: 18000}}
	return s
}

func exportLUT() einvoice.Source {
	return einvoice.Source{Number: "INV/2627/000003", IssuedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, ist), SupplyType: einvoice.ExportLUT,
		PlaceOfSupply: "OUTSIDE_INDIA", Seller: seller,
		Buyer:    einvoice.Party{LegalName: "Acme Inc", Address: "100 Main St, Austin, TX 78701, United States", Country: "US", CountryName: "United States"},
		Currency: "USD", ForeignTotal: 120000, // USD 1,200.00 at 83.25
		Lines: []einvoice.Line{{Description: "Pro plan, annual", SAC: "998315", Qty: 1, Taxable: 9990000, IGSTRateBP: 0}}}
}

func exportIGST() einvoice.Source {
	s := exportLUT()
	s.Number, s.SupplyType = "INV/2627/000004", einvoice.ExportIGST
	s.Lines = []einvoice.Line{{Description: "Pro plan, annual", SAC: "998315", Qty: 1, Taxable: 9990000, IGSTRateBP: 1800, IGST: 1798200}}
	return s
}

func build(t *testing.T, s einvoice.Source) []byte {
	t.Helper()
	doc, err := einvoice.Build(s)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	raw, err := einvoice.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/invoice/einvoice -update` and review the file)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from %s\n--- got ---\n%s\n--- want ---\n%s", name, path, got, want)
	}
}

func TestGoldenDocuments(t *testing.T) {
	for name, src := range map[string]einvoice.Source{
		"intra_b2b": intraB2B(), "inter_b2b": interB2B(), "export_lut": exportLUT(), "export_igst": exportIGST(),
	} {
		raw := build(t, src)
		golden(t, name, raw)
		if err := einvoice.Validate(raw); err != nil {
			t.Errorf("%s does not pass validation: %v", name, err)
		}
		if !json.Valid(raw) {
			t.Errorf("%s is not JSON", name)
		}
	}
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func at(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v: not an object at %q", path, p)
		}
		cur = mm[p]
	}
	return cur
}

func TestMappingOfTheFieldsThatMatter(t *testing.T) {
	intra := decode(t, build(t, intraB2B()))
	for path, want := range map[string]any{
		"Version": "1.1", "TranDtls.TaxSch": "GST", "TranDtls.SupTyp": "B2B", "TranDtls.RegRev": "N", "TranDtls.IgstOnIntra": "N",
		"DocDtls.Typ": "INV", "DocDtls.No": "INV/2627/000001", "DocDtls.Dt": "17/10/2026",
		"SellerDtls.Gstin": "27AAPFU0939F1ZV", "SellerDtls.LglNm": "Voice Studio Pvt Ltd", "SellerDtls.TrdNm": "Voicestudio",
		"SellerDtls.Pin": float64(400020), "SellerDtls.Stcd": "27", "SellerDtls.Loc": "Mumbai", "SellerDtls.Ph": "919820012345", "SellerDtls.Em": "help@example.com",
		"BuyerDtls.Gstin": "27AABCU9603R1ZX", "BuyerDtls.Pos": "27", "BuyerDtls.Stcd": "27", "BuyerDtls.Pin": float64(400050), "BuyerDtls.Loc": "Mumbai",
		"ValDtls.AssVal": json.Number("1000.00"), "ValDtls.CgstVal": json.Number("90.00"), "ValDtls.SgstVal": json.Number("90.00"), "ValDtls.IgstVal": json.Number("0.00"),
		"ValDtls.TotInvVal": json.Number("1180.00"),
	} {
		got := at(t, intra, strings.Split(path, ".")...)
		if num, ok := want.(json.Number); ok {
			f, _ := num.Float64()
			want = f
		}
		if got != want {
			t.Errorf("%s = %v (%T), want %v", path, got, got, want)
		}
	}
	items := intra["ItemList"].([]any)
	it := items[0].(map[string]any)
	if len(items) != 1 || it["SlNo"] != "1" || it["IsServc"] != "Y" || it["HsnCd"] != "998315" || it["GstRt"] != float64(18) || it["AssAmt"] != float64(1000) ||
		it["CgstAmt"] != float64(90) || it["SgstAmt"] != float64(90) || it["IgstAmt"] != float64(0) || it["TotItemVal"] != float64(1180) || it["PrdDesc"] != "Pro plan, annual" {
		t.Errorf("item %v", it)
	}
	if _, ok := intra["ExpDtls"]; ok {
		t.Error("a domestic invoice has no export details")
	}

	inter := decode(t, build(t, interB2B()))
	if at(t, inter, "BuyerDtls", "Pos") != "29" || at(t, inter, "BuyerDtls", "Stcd") != "29" || at(t, inter, "BuyerDtls", "Pin") != float64(560025) ||
		at(t, inter, "BuyerDtls", "Loc") != "Bengaluru" || at(t, inter, "BuyerDtls", "Addr1") != "5 Residency Road" || at(t, inter, "BuyerDtls", "Addr2") != "Ashok Nagar" {
		t.Errorf("inter-state buyer %v", inter["BuyerDtls"])
	}
	iit := inter["ItemList"].([]any)[0].(map[string]any)
	if iit["GstRt"] != float64(18) || iit["IgstAmt"] != float64(180) || iit["CgstAmt"] != float64(0) || at(t, inter, "ValDtls", "IgstVal") != float64(180) {
		t.Errorf("inter-state item %v", iit)
	}

	exp := decode(t, build(t, exportLUT()))
	for path, want := range map[string]any{
		"TranDtls.SupTyp": "EXPWOP", "BuyerDtls.Gstin": "URP", "BuyerDtls.Pos": "96", "BuyerDtls.Stcd": "96", "BuyerDtls.Pin": float64(999999),
		"ExpDtls.ForCur": "USD", "ExpDtls.CntCode": "US", "ExpDtls.RefClm": "N", "ValDtls.TotInvVal": float64(99900), "ValDtls.TotInvValFc": float64(1200),
		"ValDtls.IgstVal": float64(0),
	} {
		if got := at(t, exp, strings.Split(path, ".")...); got != want {
			t.Errorf("export %s = %v, want %v", path, got, want)
		}
	}
	if at(t, exp, "TranDtls", "SupTyp") == "EXPWP" {
		t.Error("an export under LUT pays no IGST")
	}
	expIGST := decode(t, build(t, exportIGST()))
	if at(t, expIGST, "TranDtls", "SupTyp") != "EXPWP" || at(t, expIGST, "ValDtls", "IgstVal") != float64(17982) || at(t, expIGST, "ValDtls", "TotInvVal") != float64(117882) {
		t.Errorf("export with IGST: %v", expIGST["ValDtls"])
	}
}

func TestAddressesAreSplitIntoLineLocalityAndPin(t *testing.T) {
	cases := map[string]struct {
		addr, addr1, loc string
		pin              float64
	}{
		"street, area, city, state pin":   {"1 Marine Drive, Churchgate, Mumbai, Maharashtra 400020", "1 Marine Drive, Churchgate", "Mumbai", 400020},
		"street, city pin":                {"22 Linking Road, Bandra West, Mumbai 400050", "22 Linking Road, Bandra West", "Mumbai", 400050},
		"pin in its own segment":          {"12 MG Road, Pune, 411001", "12 MG Road", "Pune", 411001},
		"pin first":                       {"560025, 5 Residency Road, Bengaluru", "5 Residency Road", "Bengaluru", 560025},
		"one segment":                     {"Rajiv Chowk New Delhi 110001", "Rajiv Chowk New Delhi", "Rajiv Chowk New Delhi", 110001},
		"extra spaces and a trailing dot": {"  7  Park St ,  Kolkata   700016. ", "7 Park St", "Kolkata", 700016},
	}
	for name, tc := range cases {
		s := intraB2B()
		s.Buyer.Address = tc.addr
		doc := decode(t, build(t, s))
		b := doc["BuyerDtls"].(map[string]any)
		if b["Addr1"] != tc.addr1 || b["Loc"] != tc.loc || b["Pin"] != tc.pin {
			t.Errorf("%s: Addr1=%q Loc=%q Pin=%v, want %q %q %v", name, b["Addr1"], b["Loc"], b["Pin"], tc.addr1, tc.loc, tc.pin)
		}
	}
}

func TestProblemsAreReportedTogetherAndInPlainWords(t *testing.T) {
	bad := intraB2B()
	bad.Number = "ABCDE/2627/000001" // 17 characters
	bad.Seller.Address = "Mumbai"    // no PIN
	bad.Buyer.GSTIN = ""
	bad.Lines[0].SAC = "99"
	_, err := einvoice.Build(bad)
	var pe *einvoice.Problems
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *Problems", err)
	}
	all := strings.Join(pe.List, "\n")
	for _, want := range []string{"document number", "16 characters", "seller", "PIN", "buyer", "GSTIN", "SAC"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems lack %q:\n%s", want, all)
		}
	}
	if len(pe.List) < 4 {
		t.Errorf("want every problem listed, got %v", pe.List)
	}
}

func TestOnlyB2BAndExportInvoicesHaveAnEInvoice(t *testing.T) {
	s := intraB2B()
	s.SupplyType = "b2c"
	if _, err := einvoice.Build(s); !errors.Is(err, einvoice.ErrNotEligible) {
		t.Fatalf("b2c: %v", err)
	}
	s.SupplyType = "nonsense"
	if _, err := einvoice.Build(s); !errors.Is(err, einvoice.ErrNotEligible) {
		t.Fatalf("unknown supply type: %v", err)
	}
	if einvoice.Eligible("b2c") || !einvoice.Eligible("b2b") || !einvoice.Eligible("export_lut") || !einvoice.Eligible("export_igst") {
		t.Error("Eligible")
	}
}

func TestExportsNeedACountryAndB2BNeedsAGSTIN(t *testing.T) {
	s := exportLUT()
	s.Buyer.Country = ""
	if _, err := einvoice.Build(s); err == nil || !strings.Contains(err.Error(), "country") {
		t.Errorf("export without a country: %v", err)
	}
	s = exportLUT()
	s.ForeignTotal = 0
	if _, err := einvoice.Build(s); err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Errorf("export without the foreign amount: %v", err)
	}
	s = intraB2B()
	s.Lines = nil
	if _, err := einvoice.Build(s); err == nil {
		t.Error("an invoice without lines")
	}
}

func TestValidateRejectsDocumentsThatBreakTheSchema(t *testing.T) {
	good := build(t, interB2B())
	mutate := func(f func(m map[string]any)) []byte {
		m := decode(t, good)
		f(m)
		raw, _ := json.Marshal(m)
		return raw
	}
	seller := func(m map[string]any) map[string]any { return m["SellerDtls"].(map[string]any) }
	item := func(m map[string]any) map[string]any { return m["ItemList"].([]any)[0].(map[string]any) }
	cases := map[string]struct {
		doc  []byte
		want string
	}{
		"missing section":         {mutate(func(m map[string]any) { delete(m, "ValDtls") }), "ValDtls"},
		"missing PIN":             {mutate(func(m map[string]any) { delete(seller(m), "Pin") }), "Pin"},
		"PIN too small":           {mutate(func(m map[string]any) { seller(m)["Pin"] = 12345 }), "Pin"},
		"PIN as a string":         {mutate(func(m map[string]any) { seller(m)["Pin"] = "400020" }), "Pin"},
		"lower-case GSTIN":        {mutate(func(m map[string]any) { seller(m)["Gstin"] = "27aapfu0939f1zv" }), "Gstin"},
		"unknown supply type":     {mutate(func(m map[string]any) { m["TranDtls"].(map[string]any)["SupTyp"] = "B2C" }), "SupTyp"},
		"wrong version":           {mutate(func(m map[string]any) { m["Version"] = "1.0" }), "Version"},
		"date format":             {mutate(func(m map[string]any) { m["DocDtls"].(map[string]any)["Dt"] = "2026-10-17" }), "Dt"},
		"document number":         {mutate(func(m map[string]any) { m["DocDtls"].(map[string]any)["No"] = "inv/1" }), "No"},
		"extra field":             {mutate(func(m map[string]any) { m["Colour"] = "red" }), "Colour"},
		"extra field in a party":  {mutate(func(m map[string]any) { seller(m)["Fax"] = "1" }), "Fax"},
		"HSN too short":           {mutate(func(m map[string]any) { item(m)["HsnCd"] = "99" }), "HsnCd"},
		"three decimals in money": {mutate(func(m map[string]any) { item(m)["AssAmt"] = 1000.125 }), "AssAmt"},
		"negative amount":         {mutate(func(m map[string]any) { item(m)["IgstAmt"] = -1 }), "IgstAmt"},
		"empty item list":         {mutate(func(m map[string]any) { m["ItemList"] = []any{} }), "ItemList"},
		"not an object":           {[]byte(`[1,2]`), "object"},
		"not JSON":                {[]byte(`{`), "JSON"},
	}
	for name, tc := range cases {
		err := einvoice.Validate(tc.doc)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, tc.want)
		}
	}
	if err := einvoice.Validate(good); err != nil {
		t.Fatalf("the unmutated document: %v", err)
	}
}

func TestValidateChecksTheArithmeticTheSchemaCannot(t *testing.T) {
	good := build(t, intraB2B())
	mutate := func(f func(m map[string]any)) []byte {
		m := decode(t, good)
		f(m)
		raw, _ := json.Marshal(m)
		return raw
	}
	for name, doc := range map[string][]byte{
		"total does not add up":      mutate(func(m map[string]any) { m["ValDtls"].(map[string]any)["TotInvVal"] = 1181.0 }),
		"assessable value differs":   mutate(func(m map[string]any) { m["ValDtls"].(map[string]any)["AssVal"] = 999.0 }),
		"item total differs":         mutate(func(m map[string]any) { m["ItemList"].([]any)[0].(map[string]any)["TotItemVal"] = 1179.0 }),
		"tax total differs":          mutate(func(m map[string]any) { m["ValDtls"].(map[string]any)["CgstVal"] = 91.0 }),
		"export without its details": mutate(func(m map[string]any) { m["TranDtls"].(map[string]any)["SupTyp"] = "EXPWOP" }),
	} {
		if err := einvoice.Validate(doc); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSchemaIsEmbeddedAndNamesTheVersion(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal(einvoice.SchemaJSON(), &s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s["title"].(string), "1.1") || !strings.Contains(s["$comment"].(string), "TODO(spec)") {
		t.Errorf("schema header: %v", s["title"])
	}
}
