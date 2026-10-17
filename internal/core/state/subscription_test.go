package state

import "testing"

func TestSubscriptionTransitions(t *testing.T) {
	cases := []struct {
		from SubscriptionStatus
		ev   SubscriptionEvent
		act  Action
		to   SubscriptionStatus
	}{
		{SubIncomplete, SubSetupSucceeded, Apply, SubActive},
		{SubIncomplete, SubSetupSucceededTrial, Apply, SubTrialing},
		{SubIncomplete, SubSetupExpired, Apply, SubIncompleteExpired},
		{SubIncomplete, SubRenewalSucceeded, Reject, SubIncomplete},
		{SubTrialing, SubRenewalSucceeded, Apply, SubActive},
		{SubTrialing, SubRenewalFailed, Apply, SubPastDue},
		{SubActive, SubRenewalSucceeded, Apply, SubActive},
		{SubActive, SubRenewalFailed, Apply, SubPastDue},
		{SubActive, SubCancel, Apply, SubCanceled},
		{SubActive, SubPeriodEndCancel, Apply, SubCanceled},
		{SubActive, SubCompleted, Apply, SubEnded},
		{SubPastDue, SubRenewalSucceeded, Apply, SubActive},
		{SubPastDue, SubRenewalFailed, Apply, SubPastDue},
		{SubPastDue, SubDunningExhaustedCancel, Apply, SubCanceled},
		{SubPastDue, SubDunningExhaustedKeep, Ignore, SubPastDue},
		{SubActive, SubDunningExhaustedCancel, Ignore, SubActive},
		{SubCanceled, SubRenewalSucceeded, Ignore, SubCanceled},
		{SubCanceled, SubCancel, Ignore, SubCanceled},
		{SubEnded, SubRenewalFailed, Ignore, SubEnded},
		{SubIncompleteExpired, SubSetupSucceeded, Ignore, SubIncompleteExpired},
		{SubActive, SubSetupSucceeded, Ignore, SubActive},
	}
	for _, c := range cases {
		got := SubscriptionNext(c.from, c.ev)
		if got.Action != c.act || got.Next != c.to {
			t.Errorf("%s + %s = %s/%s, want %s/%s", c.from, c.ev, got.Action, got.Next, c.act, c.to)
		}
	}
}

func TestSubscriptionTableIsTotal(t *testing.T) {
	for _, s := range AllSubscriptionStatuses {
		for _, e := range AllSubscriptionEvents {
			out := SubscriptionNext(s, e)
			if out.Action == Apply && !validSub(out.Next) {
				t.Fatalf("%s+%s applies to unknown %q", s, e, out.Next)
			}
		}
	}
	if out := SubscriptionNext("bogus", SubCancel); out.Action != Reject {
		t.Fatal("unknown status not rejected")
	}
}

func TestTerminalSubscriptionsNeverLeave(t *testing.T) {
	for _, s := range []SubscriptionStatus{SubCanceled, SubEnded, SubIncompleteExpired} {
		for _, e := range AllSubscriptionEvents {
			if out := SubscriptionNext(s, e); out.Action == Apply {
				t.Fatalf("terminal %s left via %s", s, e)
			}
		}
	}
}

func TestMandateTransitions(t *testing.T) {
	cases := []struct {
		from MandateStatus
		ev   MandateEvent
		act  Action
		to   MandateStatus
	}{
		{MandatePending, MandateActivated, Apply, MandateActive},
		{MandatePending, MandateFailedEv, Apply, MandateFailed},
		{MandateActive, MandateRevokedEv, Apply, MandateRevoked},
		{MandateActive, MandateExpiredEv, Apply, MandateExpired},
		{MandateActive, MandateActivated, Ignore, MandateActive},
		{MandateRevoked, MandateActivated, Ignore, MandateRevoked},
		{MandatePending, MandateRevokedEv, Apply, MandateRevoked},
	}
	for _, c := range cases {
		got := MandateNext(c.from, c.ev)
		if got.Action != c.act || got.Next != c.to {
			t.Errorf("%s + %s = %s/%s, want %s/%s", c.from, c.ev, got.Action, got.Next, c.act, c.to)
		}
	}
}
