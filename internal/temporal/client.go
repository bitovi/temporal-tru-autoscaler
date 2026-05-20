// Package temporal provides a client for the Temporal Cloud API and its
// OpenMetrics v1 metrics endpoint.
//
// Temporal Cloud API reference:
//   https://docs.temporal.io/ops
//   https://github.com/temporalio/cloud-api
//
// Metrics endpoint (v1 OpenMetrics):
//   https://metrics.temporal.io/prometheus/metrics
//
// APS metric used: temporal_cloud_v1_total_action_count
// APS limit metric: temporal_cloud_v1_action_limit
//
// TRU management API (REST via Cloud Ops API):
//   GET  https://saas-api.tmprl.cloud/cloud/namespaces/{namespace}
//   POST https://saas-api.tmprl.cloud/cloud/namespaces/{namespace}
package temporal

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

const (
	// temporalCloudAPIBase is the base URL for the Temporal Cloud REST API.
	temporalCloudAPIBase = "https://saas-api.tmprl.cloud"

	// metricsEndpointBase is the Temporal Cloud OpenMetrics v1 endpoint.
	// The v0 PromQL endpoint (saas-api.tmprl.cloud/prometheus/metrics) was
	// deprecated 2026-04-02 and will be disabled 2026-10-05.
	metricsEndpointBase = "https://metrics.temporal.io/v1/metrics"

	// apsMetricName is the v1 metric for total actions per second for a namespace.
	apsMetricName = "temporal_cloud_v1_total_action_count"

	// apsLimitMetricName is the v1 metric for the configured APS limit.
	apsLimitMetricName = "temporal_cloud_v1_action_limit"

	// defaultHTTPTimeout is the timeout for all HTTP calls to Temporal Cloud.
	defaultHTTPTimeout = 30 * time.Second
)

// apsPerTRU is the number of Actions Per Second supported by one TRU.
// Source: https://docs.temporal.io/cloud/capacity-modes — "Each TRU supports up to 500 APS"
const apsPerTRU = 500

// validTRUValues are the only TRU increments the Temporal Cloud API accepts.
// Source: https://docs.temporal.io/cloud/capacity-modes
var validTRUValues = []int{2, 3, 4, 6, 8, 10, 12}

// NextValidTRU returns the smallest valid TRU value strictly greater than current.
// Returns current if it is already at the maximum.
func NextValidTRU(current int) int {
	for _, v := range validTRUValues {
		if v > current {
			return v
		}
	}
	return validTRUValues[len(validTRUValues)-1]
}

// PrevValidTRU returns the largest valid TRU value strictly less than current.
// Returns current if it is already at the minimum.
func PrevValidTRU(current int) int {
	prev := validTRUValues[0]
	for _, v := range validTRUValues {
		if v >= current {
			return prev
		}
		prev = v
	}
	return prev
}

// NamespaceInfo contains the TRU level and APS ceiling for a Temporal Cloud namespace.
type NamespaceInfo struct {
	CurrentTRU int
	// APSCeiling is CurrentTRU * apsPerTRU.
	APSCeiling float64
}

// Client communicates with the Temporal Cloud API and metrics endpoint.
type Client struct {
	httpClient        *http.Client
	metricsHTTPClient *http.Client // separate client for metrics; may use mTLS
	metricsUseMTLS    bool         // when true, skip Bearer token on metrics requests
	apiKey            string
	accountID         string
	apiBaseURL        string // overridable for tests
	metricsBaseURL    string // overridable for tests
}

// NewClient creates a new Temporal Cloud client with API key auth for all endpoints.
// accountID is the Temporal Cloud account identifier (shown in the Temporal Cloud UI).
func NewClient(apiKey, accountID string) *Client {
	base := &http.Client{Timeout: defaultHTTPTimeout}
	return &Client{
		httpClient:        base,
		metricsHTTPClient: base,
		apiKey:            apiKey,
		accountID:         accountID,
		apiBaseURL:        temporalCloudAPIBase,
		metricsBaseURL:    metricsEndpointBase,
	}
}

// NewClientWithMTLS creates a Temporal Cloud client that uses mTLS for the metrics
// endpoint. certPEM and keyPEM are PEM-encoded client certificate and private key.
// Use this for Temporal Cloud accounts that have an observability CA cert configured.
// The metrics URL is derived from accountID: https://{accountID}.tmprl.cloud/prometheus/metrics
func NewClientWithMTLS(apiKey, accountID string, certPEM, keyPEM []byte) (*Client, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing observability mTLS cert/key: %w", err)
	}
	metricsTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}
	// Account-specific mTLS metrics endpoint uses the PromQL API.
	// Base URL is set to the query endpoint; namespace filter is applied in the query param.
	accountMetricsURL := fmt.Sprintf("https://%s.tmprl.cloud/prometheus/api/v1/query", accountID)
	base := &http.Client{Timeout: defaultHTTPTimeout}
	return &Client{
		httpClient:        base,
		metricsHTTPClient: &http.Client{Timeout: defaultHTTPTimeout, Transport: metricsTransport},
		metricsUseMTLS:    true,
		apiKey:            apiKey,
		accountID:         accountID,
		apiBaseURL:        temporalCloudAPIBase,
		metricsBaseURL:    accountMetricsURL,
	}, nil
}

// ---------------------------------------------------------------------------
// Namespace / TRU management
// ---------------------------------------------------------------------------

// namespaceResponse is the JSON shape returned by GET /cloud/namespaces/{namespace}.
// The TRU value lives under spec.capacity_spec.provisioned.value (a float64).
// resource_version is required for optimistic concurrency control on updates.
type namespaceResponse struct {
	Namespace struct {
		Name            string `json:"name"`
		ResourceVersion string `json:"resourceVersion"`
		Spec            struct {
			CapacitySpec struct {
				Provisioned *struct {
					Value float64 `json:"value"`
				} `json:"provisioned"`
			} `json:"capacitySpec"`
		} `json:"spec"`
	} `json:"namespace"`
}

// getNamespaceRaw fetches the raw namespace response from the Temporal Cloud API.
func (c *Client) getNamespaceRaw(ctx context.Context, namespace string) (*namespaceResponse, error) {
	url := fmt.Sprintf("%s/cloud/namespaces/%s", c.apiBaseURL, namespace)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building namespace request: %w", err)
	}
	c.setAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching namespace info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("temporal API returned %d: %s", resp.StatusCode, string(body))
	}

	var nsResp namespaceResponse
	if err := json.NewDecoder(resp.Body).Decode(&nsResp); err != nil {
		return nil, fmt.Errorf("decoding namespace response: %w", err)
	}
	return &nsResp, nil
}

// GetNamespaceInfo fetches the current TRU level and derived APS ceiling from
// the Temporal Cloud API for the given namespace name.
func (c *Client) GetNamespaceInfo(ctx context.Context, namespace string) (*NamespaceInfo, error) {
	nsResp, err := c.getNamespaceRaw(ctx, namespace)
	if err != nil {
		return nil, err
	}

	provisioned := nsResp.Namespace.Spec.CapacitySpec.Provisioned
	if provisioned == nil {
		return nil, fmt.Errorf("namespace %s is not in provisioned capacity mode", namespace)
	}

	currentTRU := int(provisioned.Value)
	if currentTRU < 1 {
		currentTRU = validTRUValues[0]
	}

	return &NamespaceInfo{
		CurrentTRU: currentTRU,
		APSCeiling: float64(currentTRU) * apsPerTRU,
	}, nil
}

// updateNamespacePayload is the JSON body for POST /cloud/namespaces/{namespace}.
// resource_version is mandatory for optimistic concurrency control.
type updateNamespacePayload struct {
	Spec            updateNamespaceSpec `json:"spec"`
	ResourceVersion string              `json:"resourceVersion"`
}

type updateNamespaceSpec struct {
	CapacitySpec updateCapacitySpec `json:"capacitySpec"`
}

type updateCapacitySpec struct {
	Provisioned updateProvisioned `json:"provisioned"`
}

type updateProvisioned struct {
	// Value is the number of TRUs to provision (must be a valid TRU increment).
	Value float64 `json:"value"`
}

// SetTRU updates the TRU level for a namespace via the Temporal Cloud API.
// It fetches the current resource_version first (required for optimistic concurrency).
func (c *Client) SetTRU(ctx context.Context, namespace string, newTRU int) error {
	// Fetch current state to get the resource_version.
	nsResp, err := c.getNamespaceRaw(ctx, namespace)
	if err != nil {
		return fmt.Errorf("fetching resource_version before TRU update: %w", err)
	}
	resourceVersion := nsResp.Namespace.ResourceVersion

	url := fmt.Sprintf("%s/cloud/namespaces/%s", c.apiBaseURL, namespace)

	payload := updateNamespacePayload{
		Spec: updateNamespaceSpec{
			CapacitySpec: updateCapacitySpec{
				Provisioned: updateProvisioned{Value: float64(newTRU)},
			},
		},
		ResourceVersion: resourceVersion,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling TRU update payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("building TRU update request: %w", err)
	}
	c.setAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling TRU update API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("temporal API returned %d on TRU update: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// ---------------------------------------------------------------------------
// Prometheus metrics (APS)
// ---------------------------------------------------------------------------

// GetCurrentAPS queries the Temporal Cloud metrics endpoint and returns the
// current Actions Per Second for the given namespace.
// For API key mode it uses the OpenMetrics v1 scrape endpoint (text format).
// For mTLS mode it uses the account-specific PromQL API (JSON format).
func (c *Client) GetCurrentAPS(ctx context.Context, namespace string) (float64, error) {
	if c.metricsUseMTLS {
		return c.getCurrentAPSViaPromQL(ctx, namespace)
	}
	return c.getCurrentAPSViaOpenMetrics(ctx, namespace)
}

// getCurrentAPSViaOpenMetrics fetches APS from the global OpenMetrics v1 endpoint.
func (c *Client) getCurrentAPSViaOpenMetrics(ctx context.Context, namespace string) (float64, error) {
	url := fmt.Sprintf("%s?namespace=%s", c.metricsBaseURL, namespace)
	if c.accountID != "" {
		url = fmt.Sprintf("%s&account_id=%s", url, c.accountID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("building metrics request: %w", err)
	}
	c.setAuthHeader(req)
	req.Header.Set("Accept", "text/plain; version=0.0.4")

	resp, err := c.metricsHTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetching metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("metrics endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	return parseAPSFromMetrics(resp.Body, namespace)
}

// promQLResponse is the JSON shape returned by the Prometheus query API.
type promQLResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]json.RawMessage `json:"value"` // [timestamp, value_string]
		} `json:"result"`
	} `json:"data"`
}

// getCurrentAPSViaPromQL fetches APS from the account-specific PromQL API (mTLS).
// It tries the v0 metric name first, then v1.
func (c *Client) getCurrentAPSViaPromQL(ctx context.Context, namespace string) (float64, error) {
	for _, metricName := range []string{"temporal_cloud_v0_total_action_count", apsMetricName} {
		query := fmt.Sprintf(`%s{namespace="%s"}`, metricName, namespace)
		url := fmt.Sprintf("%s?query=%s", c.metricsBaseURL, query)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, fmt.Errorf("building PromQL request: %w", err)
		}

		resp, err := c.metricsHTTPClient.Do(req)
		if err != nil {
			return 0, fmt.Errorf("fetching PromQL metrics: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("PromQL endpoint returned %d: %s", resp.StatusCode, string(body))
		}

		var pr promQLResponse
		if err := json.Unmarshal(body, &pr); err != nil {
			return 0, fmt.Errorf("parsing PromQL response: %w", err)
		}

		if pr.Status != "success" {
			return 0, fmt.Errorf("PromQL query returned status %q", pr.Status)
		}

		var totalAPS float64
		for _, r := range pr.Data.Result {
			if ns, ok := r.Metric["namespace"]; ok && ns != namespace {
				continue
			}
			var valStr string
			if err := json.Unmarshal(r.Value[1], &valStr); err != nil {
				continue
			}
			var v float64
			if _, err := fmt.Sscanf(valStr, "%f", &v); err == nil {
				totalAPS += v
			}
		}

		if len(pr.Data.Result) > 0 || totalAPS > 0 {
			return totalAPS, nil
		}
		// No results for this metric name; try the next one.
	}

	// No data for either metric name — namespace has zero traffic.
	return 0, nil
}

// parseAPSFromMetrics parses Prometheus text-format metrics and extracts the APS
// value for the given namespace from temporal_cloud_v1_total_action_count.
func parseAPSFromMetrics(r io.Reader, namespace string) (float64, error) {
	parser := expfmt.TextParser{}
	metricFamilies, err := parser.TextToMetricFamilies(r)
	if err != nil && len(metricFamilies) == 0 {
		return 0, fmt.Errorf("parsing prometheus metrics: %w", err)
	}

	family, ok := metricFamilies[apsMetricName]
	if !ok {
		// No metric present means zero traffic for this namespace.
		return 0, nil
	}

	var totalAPS float64
	for _, m := range family.GetMetric() {
		if !metricMatchesNamespace(m, namespace) {
			continue
		}
		switch family.GetType() {
		case dto.MetricType_GAUGE:
			totalAPS += m.GetGauge().GetValue()
		case dto.MetricType_COUNTER:
			totalAPS += m.GetCounter().GetValue()
		case dto.MetricType_UNTYPED:
			totalAPS += m.GetUntyped().GetValue()
		}
	}

	return totalAPS, nil
}

// metricMatchesNamespace returns true if the metric has a "namespace" label equal
// to the given value, or if no namespace label is present (endpoint is pre-filtered).
func metricMatchesNamespace(m *dto.Metric, namespace string) bool {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == "namespace" {
			return lp.GetValue() == namespace
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (c *Client) setAuthHeader(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
}
