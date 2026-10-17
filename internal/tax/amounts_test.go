package tax

import "testing"

func TestFromTotalInclusive(t *testing.T) {
	cases := []struct {
		name       string
		total      int64
		rate       int
		inter      bool
		zero       bool
		want       Amounts
	}{
		{"intra 18% 499.00", 49900, 1800, false, false, Amounts{Taxable: 42288, CGST: 3806, SGST: 3806, Total: 49900}},
		{"inter 18% 499.00", 49900, 1800, true, false, Amounts{Taxable: 42288, IGST: 7612, Total: 49900}},
		{"odd paise split", 100, 1800, false, false, Amounts{Taxable: 85, CGST: 7, SGST: 8, Total: 100}},
		{"zero rated export", 49900, 1800, true, true, Amounts{Taxable: 49900, Total: 49900}},
		{"zero amount", 0, 1800, false, false, Amounts{}},
		{"1 paisa", 1, 1800, false, false, Amounts{Taxable: 1, Total: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FromTotal(c.total, c.rate, c.inter, c.zero)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
			if got.Taxable+got.CGST+got.SGST+got.IGST != got.Total {
				t.Fatalf("components do not sum to total: %+v", got)
			}
		})
	}
}

func TestFromTotalAlwaysSums(t *testing.T) {
	for total := int64(0); total < 5000; total += 7 {
		for _, rate := range []int{0, 500, 1200, 1800, 2800} {
			for _, inter := range []bool{false, true} {
				a, err := FromTotal(total, rate, inter, false)
				if err != nil {
					t.Fatal(err)
				}
				if a.Taxable+a.CGST+a.SGST+a.IGST != total || a.Total != total {
					t.Fatalf("total=%d rate=%d inter=%v: %+v", total, rate, inter, a)
				}
				if inter && (a.CGST != 0 || a.SGST != 0) || !inter && a.IGST != 0 {
					t.Fatalf("wrong tax heads: %+v", a)
				}
			}
		}
	}
}

func TestFromTaxableExclusive(t *testing.T) {
	got, err := FromTaxable(42288, 1800, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := Amounts{Taxable: 42288, CGST: 3806, SGST: 3806, Total: 49900}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestAmountLimits(t *testing.T) {
	if _, err := FromTotal(-1, 1800, false, false); err == nil {
		t.Fatal("negative total accepted")
	}
	if _, err := FromTotal(MaxAmount+1, 1800, false, false); err == nil {
		t.Fatal("over-limit total accepted")
	}
	if _, err := FromTotal(100, -1, false, false); err == nil {
		t.Fatal("negative rate accepted")
	}
	if _, err := FromTotal(100, 10001, false, false); err == nil {
		t.Fatal("rate over 100% accepted")
	}
}

func TestCreditSplitSumsToInvoice(t *testing.T) {
	inv, _ := FromTotal(49900, 1800, false, false)
	var prior []Amounts
	for _, r := range []int64{10000, 3333, 17, 36550} { // sums to 49900
		c, err := CreditSplit(inv, r, prior)
		if err != nil {
			t.Fatal(err)
		}
		if c.Total != r || c.Taxable+c.CGST+c.SGST+c.IGST != r {
			t.Fatalf("credit %+v does not sum to refund %d", c, r)
		}
		prior = append(prior, c)
	}
	var sum Amounts
	for _, c := range prior {
		sum = sum.Add(c)
	}
	if sum != inv {
		t.Fatalf("credits sum %+v != invoice %+v", sum, inv)
	}
	if _, err := CreditSplit(inv, 1, prior); err == nil {
		t.Fatal("credit beyond invoice total accepted")
	}
}

func TestCreditSplitFullRefundEqualsInvoice(t *testing.T) {
	inv, _ := FromTotal(123457, 1800, true, false)
	c, err := CreditSplit(inv, inv.Total, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c != inv {
		t.Fatalf("full credit %+v != invoice %+v", c, inv)
	}
}
