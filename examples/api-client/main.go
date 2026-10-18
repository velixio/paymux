// Command api-client creates one test payment using the application HTTP API.
// Set PAYMUX_URL, PAYMUX_API_KEY and PAYMUX_IDEMPOTENCY_KEY before running.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func create(ctx context.Context, client *http.Client, baseURL, key, idempotency string) (string, error) {
	if baseURL == "" || key == "" || idempotency == "" {
		return "", fmt.Errorf("set PAYMUX_URL, PAYMUX_API_KEY and a stable PAYMUX_IDEMPOTENCY_KEY")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/payments", strings.NewReader(`{"amount":49900,"currency":"INR","order_ref":"example-order","description":"Example payment"}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotency)
	response, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create payment: HTTP %d (inspect the hub request logs)", response.StatusCode)
	}
	var payment struct {
		ID          string `json:"id"`
		CheckoutURL string `json:"checkout_url"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payment); err != nil {
		return "", err
	}
	if payment.ID == "" || payment.CheckoutURL == "" {
		return "", fmt.Errorf("response is missing the payment id or checkout URL")
	}
	return payment.ID, nil
}

func main() {
	id, err := create(context.Background(), &http.Client{Timeout: 15 * time.Second}, os.Getenv("PAYMUX_URL"), os.Getenv("PAYMUX_API_KEY"), os.Getenv("PAYMUX_IDEMPOTENCY_KEY"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Keep checkout tokens out of logs. Retrieve the checkout_url in your backend
	// integration and redirect the customer; creation does not confirm collection.
	fmt.Println("Created payment:", id)
}
