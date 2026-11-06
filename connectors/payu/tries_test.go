package payu

import "testing"

func TestCardDetailsFromRealVerify(t *testing.T) {
	snap, err := txnDetail{Status: "success", Mode: "CC", Amt: "10.00", CardNo: "XXXXXXXXXXXX2346", CardType: "MAST", IssuingBank: "HDFC"}.snapshot("ref")
	if err != nil {
		t.Fatal(err)
	}
	card := snap.MethodDetails.Card
	if card == nil || card.Last4 != "2346" || card.Network != "MAST" || card.Issuer != "HDFC" {
		t.Fatalf("card = %+v", card)
	}
}

func TestNameOnCardCaptured(t *testing.T) {
	snap, err := txnDetail{Status: "failure", Mode: "CC", Amt: "10.00", CardNo: "XXXXXXXXXXXX2346", NameOnCard: " S CUSTOMER "}.snapshot("ref")
	if err != nil {
		t.Fatal(err)
	}
	if snap.MethodDetails.Card == nil || snap.MethodDetails.Card.Name != "S CUSTOMER" {
		t.Fatalf("card = %+v", snap.MethodDetails.Card)
	}
}

// The recorded PayU response has "name_on_card":null.
func TestPlaceholderNameIgnored(t *testing.T) {
	for _, name := range []flexStr{"", "  ", "null", "NA"} {
		snap, err := txnDetail{Status: "success", Mode: "CC", Amt: "10.00", CardNo: "XXXXXXXXXXXX2346", NameOnCard: name}.snapshot("ref")
		if err != nil {
			t.Fatal(err)
		}
		if snap.MethodDetails.Card.Name != "" {
			t.Errorf("%q gave name %q", name, snap.MethodDetails.Card.Name)
		}
	}
}

func TestNoCardDetailsForOtherMethods(t *testing.T) {
	snap, err := txnDetail{Status: "success", Mode: "UPI", Amt: "10.00", CardNo: "NA", IssuingBank: "HDFC"}.snapshot("ref")
	if err != nil {
		t.Fatal(err)
	}
	if snap.MethodDetails.Card != nil {
		t.Fatalf("card = %+v", snap.MethodDetails.Card)
	}
}
