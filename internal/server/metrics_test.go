package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Hesamation/guvna/internal/config"
)

func scrapeMetrics(t *testing.T, ts *httptest.Server, key string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func TestMetricsRequiresAdminKey(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"K": "k"})

	if status, _, _ := scrapeMetrics(t, ts, ""); status != http.StatusUnauthorized {
		t.Fatalf("no key: status = %d, want 401", status)
	}
	if status, _, _ := scrapeMetrics(t, ts, "client-1"); status != http.StatusUnauthorized {
		t.Fatalf("client key: status = %d, want 401", status)
	}
	status, body, hdr := scrapeMetrics(t, ts, "admin-secret")
	if status != http.StatusOK {
		t.Fatalf("admin key: status = %d, want 200", status)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("Content-Type = %q, want Prometheus exposition", ct)
	}
	if !strings.Contains(body, "guvna_uptime_seconds") {
		t.Fatalf("uptime gauge missing from scrape:\n%s", body)
	}
}

func TestMetricsRecordsChatStepsAndQuarantine(t *testing.T) {
	bad := completionUpstream(t, http.StatusTooManyRequests, `{"error":"rate limited"}`, false)
	defer bad.Close()
	good := completionUpstream(t, http.StatusOK, `{"choices":[{}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`, false)
	defer good.Close()

	providers := []config.Provider{
		{Name: "a", Type: "openai", BaseURL: bad.URL, KeyEnv: "KEY_A"},
		{Name: "b", Type: "openai", BaseURL: good.URL, KeyEnv: "KEY_B"},
	}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a"},
		{Provider: "b", Model: "m-b"},
	}}}
	_, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k", "KEY_B": "k"})

	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200", resp.StatusCode)
	}
	io.ReadAll(resp.Body)

	_, body, _ := scrapeMetrics(t, ts, "admin-secret")
	// 429 consumes all 3 attempts (initial + 2 retries), each quarantining.
	want := []string{
		`guvna_chat_requests_total{chain="main",outcome="success"} 1`,
		`guvna_chain_fallbacks_total{chain="main"} 1`,
		`guvna_step_attempts_total{model="m-a",outcome="rate_limited",provider="a"} 3`,
		`guvna_step_attempts_total{model="m-b",outcome="success",provider="b"} 1`,
		`guvna_quarantine_events_total{class="rate_limit",provider="a"} 3`,
		`guvna_tokens_total{direction="in",model="m-b",provider="b"} 5`,
		`guvna_tokens_total{direction="out",model="m-b",provider="b"} 9`,
		`guvna_keypool_keys{provider="a",state="quarantined"} 1`,
		`guvna_keypool_keys{provider="b",state="healthy"} 1`,
		`guvna_step_duration_seconds_count{model="m-a",provider="a"} 3`,
		`guvna_step_duration_seconds_count{model="m-b",provider="b"} 1`,
		`guvna_chat_duration_seconds_count{chain="main"} 1`,
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("missing metric line %q in scrape:\n%s", line, body)
		}
	}
}

func TestMetricsRecordsRejectionWithoutClientLabels(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"K": "k"})

	resp := doChat(t, ts, "definitely-not-a-real-model-12345", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat status = %d, want 404", resp.StatusCode)
	}
	io.ReadAll(resp.Body)

	_, body, _ := scrapeMetrics(t, ts, "admin-secret")
	if !strings.Contains(body, `guvna_chat_requests_total{chain="unknown",outcome="rejected"} 1`) {
		t.Fatalf("rejection not counted in scrape:\n%s", body)
	}
	if strings.Contains(body, "definitely-not-a-real-model-12345") {
		t.Fatalf("client-controlled model string leaked into metric labels:\n%s", body)
	}
}

func TestMetricsRecordsAuthQuarantine(t *testing.T) {
	denied := completionUpstream(t, http.StatusUnauthorized, `{"error":"bad key"}`, false)
	defer denied.Close()

	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: denied.URL, KeyEnv: "KEY_A"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m-a"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k"})

	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("chat status = %d, want 401", resp.StatusCode)
	}
	io.ReadAll(resp.Body)

	_, body, _ := scrapeMetrics(t, ts, "admin-secret")
	want := []string{
		`guvna_chat_requests_total{chain="main",outcome="failed"} 1`,
		`guvna_step_attempts_total{model="m-a",outcome="auth_error",provider="a"} 3`,
		`guvna_quarantine_events_total{class="auth",provider="a"} 3`,
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("missing metric line %q in scrape:\n%s", line, body)
		}
	}
}
