package subscriptions

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"text/template"
	"time"

	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/tax"
	"github.com/velixio/paymux/internal/web"
	"github.com/velixio/paymux/pkg/connector/money"

	"github.com/velixio/paymux/internal/notify"
)

// predebitTemplate is the template file of the customer pre-debit notice. It
// is looked up in Options.Templates first (the overlay that serves
// PAYMUX_DATA_DIR/custom/templates), then in the embedded defaults.
const predebitTemplate = "templates/email_predebit.txt"

// predebitData is what the pre-debit email template sees: the contents RBI
// requires of the notice (merchant, amount, debit date, mandate reference,
// reason) plus the customer's name and how to reach the merchant.
type predebitData struct {
	Merchant     string
	CustomerName string
	Amount       string // "INR 499.00"
	DebitDate    string // "28 Feb 2027" (IST)
	MandateRef   string
	Reason       string
	SupportEmail string
}

func (s *Service) loadTemplate(lang string) (*template.Template, error) {
	var raw []byte
	var err error
	if s.templates != nil {
		raw, err = fs.ReadFile(s.templates, predebitTemplate)
	}
	if s.templates == nil || err != nil {
		if raw, err = fs.ReadFile(web.Embedded(), predebitTemplate); err != nil {
			return nil, err
		}
	}
	// {{t "key" args...}} reads the message catalogs (customer_language); an
	// override that does not use it keeps working.
	pages, err := s.pages()
	if err != nil {
		return nil, err
	}
	tr := func(key string, args ...any) string { return pages.T(lang, key, args...) }
	return template.New("predebit").Option("missingkey=error").Funcs(template.FuncMap{"t": tr}).Parse(string(raw))
}

// renderPredebit renders the template into a subject (its first line, after
// "Subject:") and a body.
func (s *Service) renderPredebit(d predebitData, lang string) (subject, body string, err error) {
	t, err := s.loadTemplate(lang)
	if err != nil {
		return "", "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return "", "", err
	}
	text := strings.ReplaceAll(buf.String(), "\r\n", "\n")
	first, rest, _ := strings.Cut(text, "\n")
	subject, ok := strings.CutPrefix(first, "Subject:")
	if !ok {
		return "Upcoming automatic payment to " + d.Merchant, text, nil
	}
	// A subject must be a single line.
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)), strings.TrimLeft(rest, "\n"), nil
}

// sendPredebitEmail sends the customer a notice of the upcoming debit, in
// addition to the provider's own. It does nothing unless the predebit_email
// setting is on and the customer has an email address. A failure is logged and
// otherwise ignored: the provider's notice is the one that counts legally.
func (s *Service) sendPredebitEmail(ctx context.Context, subID string, amount int64, debitAt time.Time) {
	if s.mailer == nil {
		return
	}
	var (
		sub  store.Subscription
		cust store.Customer
		m    store.Mandate
		name = ""
		supp string
		on   = true
		lang = "en"
	)
	err := s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if sub, err = store.GetSubscription(ctx, q, subID); err != nil {
			return err
		}
		v, err := settings.ResolveWith(ctx, q, sub.AppID, "predebit_email")
		if err != nil {
			return err
		}
		if err := v.Decode(&on); err != nil {
			return err
		}
		if !on {
			return nil
		}
		if lv, err := settings.ResolveWith(ctx, q, sub.AppID, "customer_language"); err == nil {
			_ = lv.Decode(&lang)
		}
		if cust, err = store.GetCustomer(ctx, q, sub.CustomerID); err != nil {
			return err
		}
		if m, err = store.GetMandate(ctx, q, sub.MandateID); err != nil {
			return err
		}
		if app, err := store.GetApp(ctx, q, sub.AppID); err == nil {
			name = app.Name
		}
		// The merchant on the customer's statement is the legal entity that issues the invoices.
		if prof, err := store.GetAppTaxProfile(ctx, q, sub.AppID); err == nil {
			if te, err := store.GetTaxEntity(ctx, q, prof.TaxEntityID); err == nil {
				name, supp = firstNonEmpty(te.TradeName, firstNonEmpty(te.LegalName, name)), te.SupportEmail
			}
		}
		return nil
	})
	if err != nil {
		s.warn("pre-debit email: loading", err)
		return
	}
	if !on || strings.TrimSpace(cust.Email) == "" {
		return
	}
	amt, err := money.FormatDecimal(amount, sub.Currency)
	if err != nil {
		s.warn("pre-debit email: amount", err)
		return
	}
	pages, err := s.pages()
	if err != nil {
		s.warn("pre-debit email: pages", err)
		return
	}
	if !pages.HasLang(lang) {
		lang = "en"
	}
	reason := pages.T(lang, "predebit.reason_renewal")
	if sub.Description != "" {
		reason += ": " + sub.Description
	}
	subject, body, err := s.renderPredebit(predebitData{
		Merchant: firstNonEmpty(name, pages.T(lang, "predebit.fallback_merchant")), CustomerName: firstNonEmpty(cust.Name, pages.T(lang, "predebit.fallback_customer")), Amount: sub.Currency + " " + amt,
		DebitDate: debitAt.In(tax.IST).Format("02 Jan 2006"), MandateRef: firstNonEmpty(m.ProviderMandateID, m.ProviderRef),
		Reason: reason, SupportEmail: supp,
	}, lang)
	if err != nil {
		s.warn("pre-debit email: template", err)
		return
	}
	if err := s.mailer.Send(ctx, notify.Message{To: []string{cust.Email}, Subject: subject, Text: body}); err != nil {
		s.warn("pre-debit email: sending", err)
	}
}

func (s *Service) warn(msg string, err error) {
	if s.log != nil {
		s.log.Warn("subscriptions: "+msg, "err", err)
	}
}
