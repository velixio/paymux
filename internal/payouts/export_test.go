package payouts

// SetReviewPageSize makes inReview read that many payouts per page, and
// returns the function that restores the default.
func SetReviewPageSize(n int) (restore func()) {
	old := reviewPageSize
	reviewPageSize = n
	return func() { reviewPageSize = old }
}
