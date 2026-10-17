package state

import "testing"

// pcase is one cell of the payment transition table: from + event gives
// action, next status, and the late/reason annotations.
type pcase struct {
	from   PaymentStatus
	ev     PaymentEvent
	action Action
	next   PaymentStatus
	late   bool
	reason string
}

func TestPaymentNextExhaustive(t *testing.T) {
	const (
		cr, ra, pr, su, fa, ex, ca, nr = PaymentCreated, PaymentRequiresAction, PaymentProcessing, PaymentSucceeded,
			PaymentFailed, PaymentExpired, PaymentCanceled, PaymentNeedsReview
	)
	const (
		eRA, eProc, eSucc, eMis, eFail, eExp, eCan = EvRequiresAction, EvProcessing, EvSucceeded, EvSucceededMismatch,
			EvFailed, EvExpired, EvCancel
	)
	want := []pcase{
		// created
		{cr, eRA, Apply, ra, false, ""}, {cr, eProc, Apply, pr, false, ""}, {cr, eSucc, Apply, su, false, ""},
		{cr, eMis, Apply, nr, false, ReasonAmountMismatch}, {cr, eFail, Apply, fa, false, ""},
		{cr, eExp, Apply, ex, false, ""}, {cr, eCan, Apply, ca, false, ""},
		// requires_action
		{ra, eRA, Ignore, ra, false, ""}, {ra, eProc, Apply, pr, false, ""}, {ra, eSucc, Apply, su, false, ""},
		{ra, eMis, Apply, nr, false, ReasonAmountMismatch}, {ra, eFail, Apply, fa, false, ""},
		{ra, eExp, Apply, ex, false, ""}, {ra, eCan, Apply, ca, false, ""},
		// processing: cannot cancel or expire while the provider is working
		{pr, eRA, Ignore, pr, false, ""}, {pr, eProc, Ignore, pr, false, ""}, {pr, eSucc, Apply, su, false, ""},
		{pr, eMis, Apply, nr, false, ReasonAmountMismatch}, {pr, eFail, Apply, fa, false, ""},
		{pr, eExp, Ignore, pr, false, ""}, {pr, eCan, Reject, pr, false, ""},
		// failed: only a late success moves it
		{fa, eRA, Ignore, fa, false, ""}, {fa, eProc, Ignore, fa, false, ""}, {fa, eSucc, Apply, su, true, ""},
		{fa, eMis, Apply, nr, true, ReasonAmountMismatch}, {fa, eFail, Ignore, fa, false, ""},
		{fa, eExp, Ignore, fa, false, ""}, {fa, eCan, Reject, fa, false, ""},
		// expired: only a late success moves it
		{ex, eRA, Ignore, ex, false, ""}, {ex, eProc, Ignore, ex, false, ""}, {ex, eSucc, Apply, su, true, ""},
		{ex, eMis, Apply, nr, true, ReasonAmountMismatch}, {ex, eFail, Ignore, ex, false, ""},
		{ex, eExp, Ignore, ex, false, ""}, {ex, eCan, Reject, ex, false, ""},
		// canceled: success after cancel is never auto-accepted
		{ca, eRA, Ignore, ca, false, ""}, {ca, eProc, Ignore, ca, false, ""},
		{ca, eSucc, Apply, nr, true, ReasonSuccessAfterCancel}, {ca, eMis, Apply, nr, true, ReasonSuccessAfterCancel},
		{ca, eFail, Ignore, ca, false, ""}, {ca, eExp, Ignore, ca, false, ""}, {ca, eCan, Ignore, ca, false, ""},
		// succeeded is terminal; a second success is a duplicate
		{su, eRA, Ignore, su, false, ""}, {su, eProc, Ignore, su, false, ""}, {su, eSucc, Duplicate, su, false, ""},
		{su, eMis, Duplicate, su, false, ""}, {su, eFail, Ignore, su, false, ""}, {su, eExp, Ignore, su, false, ""},
		{su, eCan, Reject, su, false, ""},
		// needs_review is left alone by the machine; operators resolve it
		{nr, eRA, Ignore, nr, false, ""}, {nr, eProc, Ignore, nr, false, ""}, {nr, eSucc, Ignore, nr, false, ""},
		{nr, eMis, Ignore, nr, false, ""}, {nr, eFail, Ignore, nr, false, ""}, {nr, eExp, Ignore, nr, false, ""},
		{nr, eCan, Reject, nr, false, ""},
	}
	if got, all := len(want), len(AllPaymentStatuses)*len(AllPaymentEvents); got != all {
		t.Fatalf("table covers %d of %d (status, event) pairs", got, all)
	}
	seen := map[[2]string]bool{}
	for _, c := range want {
		key := [2]string{string(c.from), string(c.ev)}
		if seen[key] {
			t.Fatalf("duplicate case %v", key)
		}
		seen[key] = true
		got := PaymentNext(c.from, c.ev)
		w := PaymentOutcome{Action: c.action, Next: c.next, Late: c.late, Reason: c.reason}
		if got != w {
			t.Errorf("PaymentNext(%s, %s) = %+v, want %+v", c.from, c.ev, got, w)
		}
	}
}

func TestPaymentNextUnknownInputsReject(t *testing.T) {
	if got := PaymentNext("bogus", EvSucceeded); got.Action != Reject {
		t.Errorf("unknown status: %+v", got)
	}
	if got := PaymentNext(PaymentCreated, "bogus"); got.Action != Reject {
		t.Errorf("unknown event: %+v", got)
	}
}

func TestPaymentForwardOnlyExceptLateSuccess(t *testing.T) {
	// The only transitions out of failed, expired and canceled lead to
	// succeeded (late success) or needs_review.
	for _, from := range []PaymentStatus{PaymentFailed, PaymentExpired, PaymentCanceled} {
		for _, ev := range AllPaymentEvents {
			o := PaymentNext(from, ev)
			if o.Action != Apply {
				continue
			}
			if o.Next != PaymentSucceeded && o.Next != PaymentNeedsReview {
				t.Errorf("%s + %s moves to %s", from, ev, o.Next)
			}
			if from == PaymentCanceled && o.Next == PaymentSucceeded {
				t.Errorf("canceled must never auto-succeed (%s)", ev)
			}
		}
	}
	// succeeded never changes status.
	for _, ev := range AllPaymentEvents {
		if o := PaymentNext(PaymentSucceeded, ev); o.Action == Apply {
			t.Errorf("succeeded + %s applies %+v", ev, o)
		}
	}
}

func TestAttemptNextExhaustive(t *testing.T) {
	const (
		cr, pe, au, cp, fa = AttemptCreated, AttemptPending, AttemptAuthorized, AttemptCaptured, AttemptFailed
	)
	type c struct {
		from, to AttemptStatus
		want     AttemptOutcome
	}
	ap := AttemptOutcome{Action: Apply}
	ig := AttemptOutcome{Action: Ignore}
	late := AttemptOutcome{Action: Apply, Late: true}
	want := []c{
		{cr, cr, ig}, {cr, pe, ap}, {cr, au, ap}, {cr, cp, ap}, {cr, fa, ap},
		{pe, cr, ig}, {pe, pe, ig}, {pe, au, ap}, {pe, cp, ap}, {pe, fa, ap},
		{au, cr, ig}, {au, pe, ig}, {au, au, ig}, {au, cp, ap}, {au, fa, ap},
		{cp, cr, ig}, {cp, pe, ig}, {cp, au, ig}, {cp, cp, ig}, {cp, fa, ig},
		{fa, cr, ig}, {fa, pe, ig}, {fa, au, ig}, {fa, cp, late}, {fa, fa, ig},
	}
	if len(want) != len(AllAttemptStatuses)*len(AllAttemptStatuses) {
		t.Fatalf("table incomplete: %d cases", len(want))
	}
	for _, x := range want {
		if got := AttemptNext(x.from, x.to); got != x.want {
			t.Errorf("AttemptNext(%s, %s) = %+v, want %+v", x.from, x.to, got, x.want)
		}
	}
	if got := AttemptNext("bogus", AttemptCaptured); got.Action != Reject {
		t.Errorf("unknown from: %+v", got)
	}
	if got := AttemptNext(AttemptCreated, "bogus"); got.Action != Reject {
		t.Errorf("unknown to: %+v", got)
	}
}

func TestRefundNextExhaustive(t *testing.T) {
	const pe, su, fa = RefundPending, RefundSucceeded, RefundFailed
	type c struct {
		from, to RefundStatus
		want     Action
	}
	want := []c{
		{pe, pe, Ignore}, {pe, su, Apply}, {pe, fa, Apply},
		{su, pe, Ignore}, {su, su, Ignore}, {su, fa, Reject},
		{fa, pe, Ignore}, {fa, su, Reject}, {fa, fa, Ignore},
	}
	if len(want) != len(AllRefundStatuses)*len(AllRefundStatuses) {
		t.Fatalf("table incomplete")
	}
	for _, x := range want {
		if got := RefundNext(x.from, x.to); got != x.want {
			t.Errorf("RefundNext(%s, %s) = %v, want %v", x.from, x.to, got, x.want)
		}
	}
	if RefundNext("bogus", RefundSucceeded) != Reject || RefundNext(RefundPending, "bogus") != Reject {
		t.Error("unknown statuses must be rejected")
	}
}
