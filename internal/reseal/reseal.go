// Package reseal rewrites every sealed value in the database under the
// current master key, which is the last step of changing the master key.
//
// The steps: generate a new key; start Paymux with it as PAYMUX_MASTER_KEY and
// the old one in PAYMUX_MASTER_KEY_OLD (Paymux seals new values under the new
// key and still opens the old ones); run "paymux secrets reseal"; when it
// reports that nothing is left under an old key, remove the old key.
package reseal

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
)

// Result counts what a run did.
type Result struct {
	Resealed int // rewritten under the active key (or, in a dry run, would be)
	Current  int // already under the active key
}

// Run opens every sealed value with the box's keys and seals it again under
// the active key. If any value cannot be opened (a key is missing, a value is
// damaged, a value is bound to another row) nothing is written and the error
// names what could not be opened; values never appear in it. With dryRun
// nothing is written either way. A value that changes while the run is in
// progress makes it fail with store.ErrSealedChanged; run it again.
func Run(ctx context.Context, db *store.DB, box *secrets.Box, dryRun bool) (Result, error) {
	var res Result
	items, err := db.SealedItems(ctx)
	if err != nil {
		return res, err
	}
	var changes []store.SealedChange
	var bad []string
	for _, it := range items {
		plain, err := box.Open(it.Enc, it.AAD)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s %s (%v)", it.Kind, it.ID, err))
			continue
		}
		if id, _ := secrets.KeyIDOf(it.Enc); id == box.ActiveKeyID() {
			res.Current++
			continue
		}
		fresh, err := box.Seal(plain, it.AAD)
		if err != nil {
			return res, err
		}
		changes = append(changes, store.SealedChange{Item: it, NewEnc: fresh})
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		shown := bad
		if len(shown) > 20 {
			shown = append(shown[:20:20], fmt.Sprintf("... and %d more", len(bad)-20))
		}
		return Result{}, fmt.Errorf("%d sealed values cannot be opened with the keys given, so nothing was changed: %s. "+
			"Add the key they were sealed with to PAYMUX_MASTER_KEY_OLD", len(bad), strings.Join(shown, "; "))
	}
	res.Resealed = len(changes)
	if dryRun || len(changes) == 0 {
		return res, nil
	}
	if err := db.ReplaceSealed(ctx, changes); err != nil {
		if errors.Is(err, store.ErrSealedChanged) {
			return Result{}, fmt.Errorf("%w; nothing was changed. Stop the hub (or run again)", err)
		}
		return Result{}, err
	}
	return res, nil
}
