package connectors

import (
	"github.com/velixio/paymux/internal/metrics"
	"github.com/velixio/paymux/pkg/connector"
)

// ProviderMetrics registers the provider request metrics and returns the
// function to pass as Options.Observe:
//
//	paymux_provider_requests_total{connector,outcome}
//	paymux_provider_request_duration_seconds{connector}   (histogram)
//
// Labels are the registered connector name and the SDK's closed set of
// outcomes, so the series count is bounded by connectors x 6.
func ProviderMetrics(reg *metrics.Registry) func(name string, o connector.RequestObservation) {
	requests := reg.Counter("paymux_provider_requests_total", "HTTP requests to payment providers, by connector and outcome.", "connector", "outcome")
	latency := reg.Histogram("paymux_provider_request_duration_seconds", "Latency of HTTP requests to payment providers.", metrics.HTTPBuckets, "connector")
	return func(name string, o connector.RequestObservation) {
		requests.Inc(name, o.Outcome)
		latency.Observe(o.Duration.Seconds(), name)
	}
}
