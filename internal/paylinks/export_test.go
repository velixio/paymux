package paylinks

import (
	"context"

	"github.com/paymux/paymux/internal/core"
)

// FailReissue makes every client-secret reissue fail with err, and returns the
// function that restores it.
func FailReissue(err error) (restore func()) {
	old := reissueSecret
	reissueSecret = func(context.Context, *core.Service, string) (string, error) { return "", err }
	return func() { reissueSecret = old }
}
