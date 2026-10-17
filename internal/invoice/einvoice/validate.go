package einvoice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Validate checks a document against the committed schema and then against
// the arithmetic and cross-field rules the portal applies. The error lists
// every problem found.
func Validate(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("e-invoice is not valid JSON: %w", err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return errors.New("e-invoice must be a JSON object")
	}
	root, err := rootSchema()
	if err != nil {
		return fmt.Errorf("e-invoice schema: %w", err)
	}
	c := &checker{root: root}
	c.check(obj, root, "")
	if len(c.errs) == 0 {
		business(obj, c)
	}
	if len(c.errs) > 0 {
		return errors.New("e-invoice " + strings.Join(c.errs, "; "))
	}
	return nil
}

// ValidateBatch checks a JSON array of documents.
func ValidateBatch(raw []byte) error {
	var docs []json.RawMessage
	if err := json.Unmarshal(raw, &docs); err != nil {
		return fmt.Errorf("e-invoice batch is not a JSON array: %w", err)
	}
	var errs []string
	for i, d := range docs {
		if err := Validate(d); err != nil {
			errs = append(errs, fmt.Sprintf("document %d: %v", i+1, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// cents reads a JSON number as an exact count of hundredths.
func cents(v any) int64 {
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok {
		return 0
	}
	r.Mul(r, big.NewRat(100, 1))
	q := new(big.Int).Quo(r.Num(), r.Denom())
	return q.Int64()
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

func business(doc map[string]any, c *checker) {
	tran, val, buyer := obj(doc["TranDtls"]), obj(doc["ValDtls"]), obj(doc["BuyerDtls"])
	items, _ := doc["ItemList"].([]any)
	var ass, cgst, sgst, igst, tot int64
	for i, it := range items {
		m := obj(it)
		a, cg, sg, ig, ti := cents(m["AssAmt"]), cents(m["CgstAmt"]), cents(m["SgstAmt"]), cents(m["IgstAmt"]), cents(m["TotItemVal"])
		if want := a + cg + sg + ig; ti != want {
			c.fail(fmt.Sprintf("ItemList[%d].TotItemVal", i), "is %d.%02d but the assessable value plus the taxes is %d.%02d", ti/100, ti%100, want/100, want%100)
		}
		ass, cgst, sgst, igst, tot = ass+a, cgst+cg, sgst+sg, igst+ig, tot+ti
	}
	for _, chk := range []struct {
		field string
		want  int64
	}{{"AssVal", ass}, {"CgstVal", cgst}, {"SgstVal", sgst}, {"IgstVal", igst}} {
		if got, present := val[chk.field]; present && cents(got) != chk.want {
			c.fail("ValDtls."+chk.field, "is %d.%02d but the items add up to %d.%02d", cents(got)/100, cents(got)%100, chk.want/100, chk.want%100)
		}
	}
	if got := cents(val["TotInvVal"]); got != tot+cents(val["RndOffAmt"])+cents(val["OthChrg"]) {
		c.fail("ValDtls.TotInvVal", "is %d.%02d but the items add up to %d.%02d", got/100, got%100, tot/100, tot%100)
	}

	sup, _ := tran["SupTyp"].(string)
	switch {
	case strings.HasPrefix(sup, "EXP"):
		exp := obj(doc["ExpDtls"])
		if exp == nil || exp["ForCur"] == nil || exp["CntCode"] == nil {
			c.fail("ExpDtls", "with ForCur and CntCode is required for an export (SupTyp %s)", sup)
		}
		if buyer["Gstin"] != "URP" || buyer["Pos"] != "96" {
			c.fail("BuyerDtls", "an export buyer is URP with place of supply 96")
		}
		if sup == "EXPWOP" && igst != 0 {
			c.fail("ValDtls.IgstVal", "an export without payment of tax (EXPWOP) must have no IGST")
		}
	case sup == "B2B":
		if buyer["Gstin"] == "URP" {
			c.fail("BuyerDtls.Gstin", "a B2B invoice needs the buyer's GSTIN, not URP")
		}
	}
}
