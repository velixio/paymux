package invoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/invoice/pades"
	"github.com/paymux/paymux/internal/store"
)

// A tax entity may hold a digital signature certificate (DSC): the PKCS#12
// file the certifying authority issues, and its password. It is kept sealed.
// While one is held, every PDF rendered for the entity is signed (PAdES); the
// printed signatory note remains the fallback when there is none, or when the
// certificate stops working, in which case an operator is alerted.

// maxDSCFile bounds an uploaded PKCS#12 file; real ones are 2 to 6 KB.
const maxDSCFile = 64 << 10

// AlertDSCUnusable is raised when a PDF could not be signed.
const AlertDSCUnusable = "tax.dsc_unusable"

type sealedDSC struct {
	P12      []byte `json:"p12"`
	Password string `json:"password"`
}

func dscAAD(entityID string) []byte { return []byte("tax_entities.dsc_enc:" + entityID) }

// SetTaxEntityDSC validates and stores a PKCS#12 certificate for the entity,
// replacing any earlier one. The file is opened first: a wrong password, an
// encryption Paymux cannot read, an unsupported key, or a certificate that is
// expired or not yet valid, is refused (ErrInvalid) and nothing is stored.
func (s *Service) SetTaxEntityDSC(ctx context.Context, actor audit.Actor, entityID string, file []byte, password string) (pades.Info, error) {
	if s.box == nil {
		return pades.Info{}, errors.New("invoice: no encryption key is configured, so a certificate cannot be stored")
	}
	if len(file) == 0 {
		return pades.Info{}, invalid("the certificate file is empty")
	}
	if len(file) > maxDSCFile {
		return pades.Info{}, invalid("the certificate file is too large (more than %d KB): a PKCS#12 file of a signature certificate is a few KB", maxDSCFile>>10)
	}
	signer, err := pades.New(file, password, s.now)
	if err != nil {
		return pades.Info{}, invalid("%s", err.Error())
	}
	info := signer.Info()
	plain, err := json.Marshal(sealedDSC{P12: file, Password: password})
	if err != nil {
		return pades.Info{}, err
	}
	enc, err := s.box.Seal(plain, dscAAD(entityID))
	if err != nil {
		return pades.Info{}, err
	}
	now := s.now().UTC()
	err = s.db.Write(ctx, func(tx store.Tx) error {
		cur, err := store.GetTaxEntity(ctx, tx, entityID)
		if errors.Is(err, store.ErrNotFound) {
			return invalid("no tax entity %q", entityID)
		}
		if err != nil {
			return err
		}
		if err := store.SetTaxEntityDSC(ctx, tx, entityID, store.EntityDSC{Enc: enc, Subject: info.Subject, Fingerprint: info.Fingerprint,
			NotAfter: info.NotAfter, UploadedAt: now}, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: cur.OrgID, Actor: actor, Action: "tax_entity.dsc_set", Target: entityID,
			Before: map[string]string{"subject": cur.DSCSubject},
			After:  map[string]string{"subject": info.Subject, "issuer": info.Issuer, "fingerprint": info.Fingerprint, "not_after": info.NotAfter.UTC().Format("2006-01-02")}})
	})
	return info, err
}

// ClearTaxEntityDSC removes the entity's certificate; documents are signed with
// the printed note again from now on.
func (s *Service) ClearTaxEntityDSC(ctx context.Context, actor audit.Actor, entityID string) error {
	return s.db.Write(ctx, func(tx store.Tx) error {
		cur, err := store.GetTaxEntity(ctx, tx, entityID)
		if errors.Is(err, store.ErrNotFound) {
			return invalid("no tax entity %q", entityID)
		}
		if err != nil {
			return err
		}
		if err := store.ClearTaxEntityDSC(ctx, tx, entityID, s.now().UTC()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: cur.OrgID, Actor: actor, Action: "tax_entity.dsc_clear", Target: entityID,
			Before: map[string]string{"subject": cur.DSCSubject}})
	})
}

// signerFor opens the entity's certificate; nil, nil when it has none.
func (s *Service) signerFor(ctx context.Context, entityID string) (*pades.Signer, error) {
	if s.box == nil || entityID == "" {
		return nil, nil
	}
	var d store.EntityDSC
	var ok bool
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { d, ok, err = store.GetTaxEntityDSC(ctx, q, entityID); return }); err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	plain, err := s.box.Open(d.Enc, dscAAD(entityID))
	if err != nil {
		return nil, fmt.Errorf("opening the certificate of %s: %w", entityID, err)
	}
	var sealed sealedDSC
	if err := json.Unmarshal(plain, &sealed); err != nil {
		return nil, err
	}
	return pades.New(sealed.P12, sealed.Password, s.now)
}
