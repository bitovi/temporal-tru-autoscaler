package temporal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// NextValidTRU / PrevValidTRU
// ---------------------------------------------------------------------------

func TestNextValidTRU(t *testing.T) {
	cases := []struct {
		current int
		want    int
	}{
		{0, 2},  // below minimum → first valid
		{1, 2},  // below minimum → first valid
		{2, 3},  // exact valid → next
		{3, 4},
		{4, 6},  // skips 5
		{5, 6},  // invalid input → next valid
		{6, 8},  // skips 7
		{7, 8},
		{8, 10},
		{9, 10},
		{10, 12},
		{11, 12},
		{12, 12}, // already at max → clamp
		{99, 12}, // beyond max → clamp
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("current=%d", tc.current), func(t *testing.T) {
			got := NextValidTRU(tc.current)
			if got != tc.want {
				t.Errorf("NextValidTRU(%d) = %d, want %d", tc.current, got, tc.want)
			}
		})
	}
}

func TestPrevValidTRU(t *testing.T) {
	cases := []struct {
		current int
		want    int
	}{
		{0, 2},  // below minimum → first valid (nothing smaller exists)
		{1, 2},
		{2, 2},  // already at min → clamp
		{3, 2},
		{4, 3},
		{5, 4},  // invalid input → previous valid
		{6, 4},  // skips 5
		{7, 6},
		{8, 6},  // skips 7
		{9, 8},
		{10, 8},
		{11, 10},
		{12, 10},
		{99, 12}, // beyond max → last valid
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("current=%d", tc.current), func(t *testing.T) {
			got := PrevValidTRU(tc.current)
			if got != tc.want {
				t.Errorf("PrevValidTRU(%d) = %d, want %d", tc.current, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseAPSFromMetrics
// ---------------------------------------------------------------------------

func TestParseAPSFromMetrics_Gauge(t *testing.T) {
	body := fmt.Sprintf(`# HELP %s Total actions per second
# TYPE %s gauge
%s{namespace="my-ns"} 350.5
`, apsMetricName, apsMetricName, apsMetricName)

	got, err := parseAPSFromMetrics(strings.NewReader(body), "my-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 350.5 {
		t.Errorf("got %.1f, want 350.5", got)
	}
}

func TestParseAPSFromMetrics_FiltersOtherNamespace(t *testing.T) {
	body := fmt.Sprintf(`# HELP %s Total actions per second
# TYPE %s gauge
%s{namespace="other-ns"} 999.0
%s{namespace="my-ns"} 42.0
`, apsMetricName, apsMetricName, apsMetricName, apsMetricName)

	got, err := parseAPSFromMetrics(strings.NewReader(body), "my-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42.0 {
		t.Errorf("got %.1f, want 42.0", got)
	}
}

func TestParseAPSFromMetrics_MetricAbsent(t *testing.T) {
	body := `# HELP some_other_metric irrelevant
# TYPE some_other_metric gauge
some_other_metric{namespace="my-ns"} 1.0
`
	got, err := parseAPSFromMetrics(strings.NewReader(body), "my-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("got %.1f, want 0 (metric absent = zero traffic)", got)
	}
}

func TestParseAPSFromMetrics_NoNamespaceLabel(t *testing.T) {
	// Endpoint pre-filtered — no namespace label on the metric.
	body := fmt.Sprintf(`# HELP %s Total actions per second
# TYPE %s gauge
%s 250.0
`, apsMetricName, apsMetricName, apsMetricName)

	got, err := parseAPSFromMetrics(strings.NewReader(body), "my-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 250.0 {
		t.Errorf("got %.1f, want 250.0", got)
	}
}

// ---------------------------------------------------------------------------
// GetNamespaceInfo
// ---------------------------------------------------------------------------

func TestGetNamespaceInfo_Provisioned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/cloud/namespaces/my-ns") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		resp := map[string]any{
			"namespace": map[string]any{
				"name":            "my-ns",
				"resourceVersion": "abc123",
				"spec": map[string]any{
					"capacitySpec": map[string]any{
						"provisioned": map[string]any{
							"value": 4.0,
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	info, err := c.GetNamespaceInfo(context.Background(), "my-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.CurrentTRU != 4 {
		t.Errorf("CurrentTRU = %d, want 4", info.CurrentTRU)
	}
	if info.APSCeiling != 4*apsPerTRU {
		t.Errorf("APSCeiling = %.0f, want %d", info.APSCeiling, 4*apsPerTRU)
	}
}

func TestGetNamespaceInfo_OnDemand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"namespace": map[string]any{
				"name":            "my-ns",
				"resourceVersion": "v1",
				"spec": map[string]any{
					"capacitySpec": map[string]any{
						// provisioned is absent — on-demand mode
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.GetNamespaceInfo(context.Background(), "my-ns")
	if err == nil {
		t.Fatal("expected error for on-demand namespace, got nil")
	}
}

func TestGetNamespaceInfo_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.GetNamespaceInfo(context.Background(), "my-ns")
	if err == nil {
		t.Fatal("expected error for non-200 response, got nil")
	}
}

// ---------------------------------------------------------------------------
// SetTRU
// ---------------------------------------------------------------------------

func TestSetTRU_SendsCorrectRequest(t *testing.T) {
	const wantResourceVersion = "rv-42"
	const wantTRU = 6

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			// First call: GET to read resourceVersion.
			if r.Method != http.MethodGet {
				t.Errorf("call 1: want GET, got %s", r.Method)
			}
			resp := map[string]any{
				"namespace": map[string]any{
					"name":            "my-ns",
					"resourceVersion": wantResourceVersion,
					"spec": map[string]any{
						"capacitySpec": map[string]any{
							"provisioned": map[string]any{"value": 4.0},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case 2:
			// Second call: POST to update TRU.
			if r.Method != http.MethodPost {
				t.Errorf("call 2: want POST, got %s", r.Method)
			}
			var body updateNamespacePayload
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding body: %v", err)
			}
			if body.ResourceVersion != wantResourceVersion {
				t.Errorf("resource_version = %q, want %q", body.ResourceVersion, wantResourceVersion)
			}
			if body.Spec.CapacitySpec.Provisioned.Value != wantTRU {
				t.Errorf("provisioned.value = %.0f, want %d", body.Spec.CapacitySpec.Provisioned.Value, wantTRU)
			}
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.SetTRU(context.Background(), "my-ns", wantTRU); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (GET + POST), got %d", callCount)
	}
}

func TestSetTRU_APIError(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// GET succeeds.
			resp := map[string]any{
				"namespace": map[string]any{
					"name":            "my-ns",
					"resourceVersion": "rv1",
					"spec": map[string]any{
						"capacitySpec": map[string]any{
							"provisioned": map[string]any{"value": 4.0},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// POST fails.
		http.Error(w, "conflict", http.StatusConflict)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.SetTRU(context.Background(), "my-ns", 6)
	if err == nil {
		t.Fatal("expected error for non-200 POST response, got nil")
	}
}

// ---------------------------------------------------------------------------
// GetCurrentAPS
// ---------------------------------------------------------------------------

func TestGetCurrentAPS(t *testing.T) {
	metricBody := fmt.Sprintf(`# HELP %s Total actions per second
# TYPE %s gauge
%s{namespace="my-ns"} 750.0
`, apsMetricName, apsMetricName, apsMetricName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("namespace") != "my-ns" {
			t.Errorf("missing or wrong namespace query param: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, metricBody)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	c.metricsBaseURL = srv.URL // metrics endpoint == same test server for simplicity
	aps, err := c.GetCurrentAPS(context.Background(), "my-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if aps != 750.0 {
		t.Errorf("got %.1f, want 750.0", aps)
	}
}

func TestGetCurrentAPS_MetricsEndpointError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	c.metricsBaseURL = srv.URL
	_, err := c.GetCurrentAPS(context.Background(), "my-ns")
	if err == nil {
		t.Fatal("expected error for non-200 metrics response, got nil")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestClient(apiBaseURL string) *Client {
	hc := &http.Client{}
	return &Client{
		httpClient:        hc,
		metricsHTTPClient: hc,
		apiKey:            "test-key",
		accountID:         "test-account",
		apiBaseURL:        apiBaseURL,
		metricsBaseURL:    apiBaseURL, // overridden per-test when needed
	}
}
