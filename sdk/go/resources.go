package paymux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

type PaymentsService struct{ client *Client }

func (s *PaymentsService) Create(ctx context.Context, params CreatePaymentParams, options ...RequestOption) (*Payment, error) {
	cfg, err := mutationOptions(options)
	if err != nil {
		return nil, err
	}
	var out Payment
	err = s.client.doJSON(ctx, http.MethodPost, "v1/payments", nil, params, &out, &cfg)
	return &out, err
}
func (s *PaymentsService) Get(ctx context.Context, id string) (*Payment, error) {
	path, err := objectPath("payments", id)
	if err != nil {
		return nil, err
	}
	var out Payment
	err = s.client.doJSON(ctx, http.MethodGet, path, nil, nil, &out, nil)
	return &out, err
}
func (s *PaymentsService) CheckoutAction(ctx context.Context, id string, params CheckoutActionParams, options ...RequestOption) (*CheckoutAction, error) {
	path, err := objectPath("payments", id, "checkout-action")
	if err != nil {
		return nil, err
	}
	cfg, err := mutationOptions(options)
	if err != nil {
		return nil, err
	}
	var out CheckoutAction
	err = s.client.doJSON(ctx, http.MethodPost, path, nil, params, &out, &cfg)
	return &out, err
}
func (s *PaymentsService) List(ctx context.Context, params ListPaymentsParams) (*PaymentPage, error) {
	q := url.Values{}
	addPageQuery(q, params.Limit, params.StartingAfter)
	if params.Status != "" {
		q.Set("status", string(params.Status))
	}
	var out PaymentPage
	err := s.client.doJSON(ctx, http.MethodGet, "v1/payments", q, nil, &out, nil)
	return &out, err
}
func (s *PaymentsService) Cancel(ctx context.Context, id string, options ...RequestOption) (*Payment, error) {
	path, err := objectPath("payments", id, "cancel")
	if err != nil {
		return nil, err
	}
	cfg, err := mutationOptions(options)
	if err != nil {
		return nil, err
	}
	var out Payment
	err = s.client.doJSON(ctx, http.MethodPost, path, nil, nil, &out, &cfg)
	return &out, err
}

type RefundsService struct{ client *Client }

func (s *RefundsService) Create(ctx context.Context, paymentID string, params CreateRefundParams, options ...RequestOption) (*Refund, error) {
	path, err := objectPath("payments", paymentID, "refunds")
	if err != nil {
		return nil, err
	}
	cfg, err := mutationOptions(options)
	if err != nil {
		return nil, err
	}
	var out Refund
	err = s.client.doJSON(ctx, http.MethodPost, path, nil, params, &out, &cfg)
	return &out, err
}
func (s *RefundsService) Get(ctx context.Context, id string) (*Refund, error) {
	path, err := objectPath("refunds", id)
	if err != nil {
		return nil, err
	}
	var out Refund
	err = s.client.doJSON(ctx, http.MethodGet, path, nil, nil, &out, nil)
	return &out, err
}

type PaymentLinksService struct{ client *Client }

func (s *PaymentLinksService) Create(ctx context.Context, params CreatePaymentLinkParams, options ...RequestOption) (*PaymentLink, error) {
	cfg, err := mutationOptions(options)
	if err != nil {
		return nil, err
	}
	var out PaymentLink
	err = s.client.doJSON(ctx, http.MethodPost, "v1/payment_links", nil, params, &out, &cfg)
	return &out, err
}
func (s *PaymentLinksService) Get(ctx context.Context, id string) (*PaymentLink, error) {
	path, err := objectPath("payment_links", id)
	if err != nil {
		return nil, err
	}
	var out PaymentLink
	err = s.client.doJSON(ctx, http.MethodGet, path, nil, nil, &out, nil)
	return &out, err
}
func (s *PaymentLinksService) List(ctx context.Context, params ListPaymentLinksParams) (*PaymentLinkPage, error) {
	q := url.Values{}
	addPageQuery(q, params.Limit, params.StartingAfter)
	if params.Active != nil {
		q.Set("active", strconv.FormatBool(*params.Active))
	}
	var out PaymentLinkPage
	err := s.client.doJSON(ctx, http.MethodGet, "v1/payment_links", q, nil, &out, nil)
	return &out, err
}
func (s *PaymentLinksService) Deactivate(ctx context.Context, id string, options ...RequestOption) (*PaymentLink, error) {
	path, err := objectPath("payment_links", id, "deactivate")
	if err != nil {
		return nil, err
	}
	cfg, err := mutationOptions(options)
	if err != nil {
		return nil, err
	}
	var out PaymentLink
	err = s.client.doJSON(ctx, http.MethodPost, path, nil, nil, &out, &cfg)
	return &out, err
}

type SubscriptionsService struct{ client *Client }

func (s *SubscriptionsService) Create(ctx context.Context, params CreateSubscriptionParams, options ...RequestOption) (*Subscription, error) {
	cfg, err := mutationOptions(options)
	if err != nil {
		return nil, err
	}
	var out Subscription
	err = s.client.doJSON(ctx, http.MethodPost, "v1/subscriptions", nil, params, &out, &cfg)
	return &out, err
}
func (s *SubscriptionsService) Get(ctx context.Context, id string) (*Subscription, error) {
	path, err := objectPath("subscriptions", id)
	if err != nil {
		return nil, err
	}
	var out Subscription
	err = s.client.doJSON(ctx, http.MethodGet, path, nil, nil, &out, nil)
	return &out, err
}
func (s *SubscriptionsService) List(ctx context.Context, params ListSubscriptionsParams) (*SubscriptionPage, error) {
	q := url.Values{}
	addPageQuery(q, params.Limit, params.StartingAfter)
	if params.Customer != "" {
		q.Set("customer", params.Customer)
	}
	if params.Status != "" {
		q.Set("status", string(params.Status))
	}
	var out SubscriptionPage
	err := s.client.doJSON(ctx, http.MethodGet, "v1/subscriptions", q, nil, &out, nil)
	return &out, err
}
func (s *SubscriptionsService) Cancel(ctx context.Context, id string, params CancelSubscriptionParams, options ...RequestOption) (*Subscription, error) {
	return s.post(ctx, id, "cancel", params, options...)
}
func (s *SubscriptionsService) Update(ctx context.Context, id string, params UpdateSubscriptionParams, options ...RequestOption) (*Subscription, error) {
	return s.post(ctx, id, "update", params, options...)
}
func (s *SubscriptionsService) UpdatePaymentMethod(ctx context.Context, id string, params UpdatePaymentMethodParams, options ...RequestOption) (*Subscription, error) {
	return s.post(ctx, id, "payment_method", params, options...)
}
func (s *SubscriptionsService) post(ctx context.Context, id, action string, body any, options ...RequestOption) (*Subscription, error) {
	path, err := objectPath("subscriptions", id, action)
	if err != nil {
		return nil, err
	}
	cfg, err := mutationOptions(options)
	if err != nil {
		return nil, err
	}
	var out Subscription
	err = s.client.doJSON(ctx, http.MethodPost, path, nil, body, &out, &cfg)
	return &out, err
}

type InvoicesService struct{ client *Client }

func (s *InvoicesService) List(ctx context.Context, paymentID string) (*InvoicePage, error) {
	if paymentID == "" {
		return nil, errors.New("paymux: payment ID is required")
	}
	q := url.Values{"payment_id": {paymentID}}
	var out InvoicePage
	err := s.client.doJSON(ctx, http.MethodGet, "v1/invoices", q, nil, &out, nil)
	return &out, err
}
func (s *InvoicesService) Get(ctx context.Context, id string) (*Invoice, error) {
	path, err := objectPath("invoices", id)
	if err != nil {
		return nil, err
	}
	var out Invoice
	err = s.client.doJSON(ctx, http.MethodGet, path, nil, nil, &out, nil)
	return &out, err
}
func (s *InvoicesService) DownloadPDF(ctx context.Context, id string) ([]byte, error) {
	return s.client.downloadPDF(ctx, "invoices", id)
}

type CreditNotesService struct{ client *Client }

func (s *CreditNotesService) List(ctx context.Context, paymentID string) (*CreditNotePage, error) {
	if paymentID == "" {
		return nil, errors.New("paymux: payment ID is required")
	}
	q := url.Values{"payment_id": {paymentID}}
	var out CreditNotePage
	err := s.client.doJSON(ctx, http.MethodGet, "v1/credit_notes", q, nil, &out, nil)
	return &out, err
}
func (s *CreditNotesService) Get(ctx context.Context, id string) (*CreditNote, error) {
	path, err := objectPath("credit_notes", id)
	if err != nil {
		return nil, err
	}
	var out CreditNote
	err = s.client.doJSON(ctx, http.MethodGet, path, nil, nil, &out, nil)
	return &out, err
}
func (s *CreditNotesService) DownloadPDF(ctx context.Context, id string) ([]byte, error) {
	return s.client.downloadPDF(ctx, "credit_notes", id)
}

const maxPDFBody = 32 << 20

func (c *Client) downloadPDF(ctx context.Context, collection, id string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("paymux: nil context")
	}
	path, err := objectPath(collection, id, "pdf")
	if err != nil {
		return nil, err
	}
	u := c.baseURL.ResolveReference(&url.URL{Path: path})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/pdf")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paymux: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeAPIError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPDFBody+1))
	if err != nil {
		return nil, fmt.Errorf("paymux: read PDF: %w", err)
	}
	if len(b) > maxPDFBody {
		return nil, errors.New("paymux: PDF exceeds 32 MiB limit")
	}
	return b, nil
}

type PayoutsService struct{ client *Client }

func (s *PayoutsService) Create(ctx context.Context, params CreatePayoutParams, options ...RequestOption) (*Payout, error) {
	cfg, err := mutationOptions(options)
	if err != nil {
		return nil, err
	}
	var out Payout
	err = s.client.doJSON(ctx, http.MethodPost, "v1/payouts", nil, params, &out, &cfg)
	return &out, err
}
func (s *PayoutsService) Get(ctx context.Context, id string) (*Payout, error) {
	path, err := objectPath("payouts", id)
	if err != nil {
		return nil, err
	}
	var out Payout
	err = s.client.doJSON(ctx, http.MethodGet, path, nil, nil, &out, nil)
	return &out, err
}
func (s *PayoutsService) List(ctx context.Context, params ListPayoutsParams) (*PayoutPage, error) {
	q := url.Values{}
	addPageQuery(q, params.Limit, params.StartingAfter)
	if params.Status != "" {
		q.Set("status", string(params.Status))
	}
	var out PayoutPage
	err := s.client.doJSON(ctx, http.MethodGet, "v1/payouts", q, nil, &out, nil)
	return &out, err
}
