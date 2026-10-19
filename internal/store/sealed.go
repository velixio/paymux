package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/paymux/paymux/internal/secrets"
)

// The sealed values (encrypted with the master key) live in several tables.
// This file lists them in one place so that a master key rotation can read and
// rewrite every one: miss one and it becomes unreadable when the old key goes.

// ErrSealedChanged means a sealed value was changed by someone else between
// being read and being replaced.
var ErrSealedChanged = errors.New("store: a sealed value changed while it was being resealed")

// SealedItem is one sealed value and where it lives.
type SealedItem struct {
	Kind string   // the table and column, for messages
	ID   string   // the row, for messages (never the value)
	Keys []string // the row's key column values
	AAD  []byte   // the additional data it was sealed with
	Enc  []byte   // the sealed value
}

// SealedChange replaces an item's value, if it is still what was read.
type SealedChange struct {
	Item   SealedItem
	NewEnc []byte
}

type sealedSpec struct {
	kind, table, enc string
	keys             []string
	aad              func(keys []string) []byte
}

var sealedSpecs = []sealedSpec{
	{"provider_accounts.config_enc", "provider_accounts", "config_enc", []string{"id"}, func(k []string) []byte { return secrets.AADProviderConfig(k[0]) }},
	{"webhook_endpoints.signing_secret_enc", "webhook_endpoints", "signing_secret_enc", []string{"id"}, func(k []string) []byte { return secrets.AADEndpointSecret(k[0]) }},
	{"users.totp_secret_enc", "users", "totp_secret_enc", []string{"id"}, func(k []string) []byte { return secrets.AADTOTP(k[0]) }},
	{"payouts.beneficiary_enc", "payouts", "beneficiary_enc", []string{"id"}, func(k []string) []byte { return secrets.AADBeneficiary(k[0]) }},
	{"tax_entities.dsc_enc", "tax_entities", "dsc_enc", []string{"id"}, func(k []string) []byte { return secrets.AADTaxEntityDSC(k[0]) }},
	{"connector_kv.value_enc", "connector_kv", "value_enc", []string{"provider_account_id", "key"}, func(k []string) []byte { return secrets.AADConnectorKV(k[0], k[1]) }},
	{"idempotency_keys.response_enc", "idempotency_keys", "response_enc", []string{"app_id", "mode", "key"}, func(k []string) []byte { return secrets.AADIdempotency(k[0], k[1], k[2]) }},
}

// SealedItems returns every sealed value in the database.
func (db *DB) SealedItems(ctx context.Context) ([]SealedItem, error) {
	var out []SealedItem
	err := db.Read(ctx, func(q Querier) error {
		for _, sp := range sealedSpecs {
			rows, err := q.QueryContext(ctx, fmt.Sprintf(`SELECT %s, %s FROM %s WHERE %s IS NOT NULL`, strings.Join(sp.keys, ", "), sp.enc, sp.table, sp.enc))
			if err != nil {
				return fmt.Errorf("%s: %w", sp.kind, err)
			}
			for rows.Next() {
				keys := make([]string, len(sp.keys))
				dest := make([]any, 0, len(keys)+1)
				for i := range keys {
					dest = append(dest, &keys[i])
				}
				var enc []byte
				dest = append(dest, &enc)
				if err := rows.Scan(dest...); err != nil {
					rows.Close()
					return fmt.Errorf("%s: %w", sp.kind, err)
				}
				out = append(out, SealedItem{Kind: sp.kind, ID: strings.Join(keys, ":"), Keys: keys, AAD: sp.aad(keys), Enc: enc})
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", sp.kind, err)
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out, err
}

// ReplaceSealed writes the new values in one transaction. A value that is no
// longer what was read makes the whole call fail with ErrSealedChanged and
// change nothing.
func (db *DB) ReplaceSealed(ctx context.Context, changes []SealedChange) error {
	specs := map[string]sealedSpec{}
	for _, sp := range sealedSpecs {
		specs[sp.kind] = sp
	}
	return db.Write(ctx, func(tx Tx) error {
		for _, c := range changes {
			sp, ok := specs[c.Item.Kind]
			if !ok {
				return fmt.Errorf("store: unknown sealed kind %q", c.Item.Kind)
			}
			if bytes.Equal(c.Item.Enc, c.NewEnc) {
				continue
			}
			where := make([]string, len(sp.keys))
			args := []any{c.NewEnc}
			for i, k := range sp.keys {
				where[i] = k + " = ?"
				args = append(args, c.Item.Keys[i])
			}
			args = append(args, c.Item.Enc)
			res, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s AND %s = ?`, sp.table, sp.enc, strings.Join(where, " AND "), sp.enc), args...)
			if err != nil {
				return fmt.Errorf("%s %s: %w", c.Item.Kind, c.Item.ID, err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return fmt.Errorf("%w: %s %s", ErrSealedChanged, c.Item.Kind, c.Item.ID)
			}
		}
		return nil
	})
}

// SealedColumns lists the names of the columns that hold sealed values.
func SealedColumns() []string {
	out := make([]string, len(sealedSpecs))
	for i, sp := range sealedSpecs {
		out[i] = sp.enc
	}
	return out
}
