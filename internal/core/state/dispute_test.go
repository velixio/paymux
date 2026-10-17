package state

import "testing"

func TestDisputeNextExhaustive(t *testing.T) {
	const nr, ur, wo, lo, ac, cl = DisputeNeedsResponse, DisputeUnderReview, DisputeWon, DisputeLost, DisputeAccepted, DisputeClosed
	type c struct {
		from, to DisputeStatus
		want     Action
	}
	want := []c{
		{nr, nr, Ignore}, {nr, ur, Apply}, {nr, wo, Apply}, {nr, lo, Apply}, {nr, ac, Apply}, {nr, cl, Apply},
		{ur, nr, Ignore}, {ur, ur, Ignore}, {ur, wo, Apply}, {ur, lo, Apply}, {ur, ac, Apply}, {ur, cl, Apply},
	}
	for _, from := range []DisputeStatus{wo, lo, ac, cl} {
		for _, to := range AllDisputeStatuses {
			w := Reject
			if to == from || !to.Terminal() {
				w = Ignore
			}
			want = append(want, c{from, to, w})
		}
	}
	if len(want) != len(AllDisputeStatuses)*len(AllDisputeStatuses) {
		t.Fatalf("table covers %d of %d pairs", len(want), len(AllDisputeStatuses)*len(AllDisputeStatuses))
	}
	for _, x := range want {
		if got := DisputeNext(x.from, x.to); got != x.want {
			t.Errorf("DisputeNext(%s, %s) = %v, want %v", x.from, x.to, got, x.want)
		}
	}
	if DisputeNext("bogus", nr) != Reject || DisputeNext(nr, "bogus") != Reject {
		t.Error("unknown statuses must be rejected")
	}
}
