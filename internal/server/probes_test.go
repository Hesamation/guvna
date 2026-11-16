package server

// Provider-probe endpoint tests: verdict classification per failure class,
// the unset-env verdict, provider/model filtering, and the probe's core
// guarantee — it reports on state without perturbing it (no quarantine, no
// step health marks, no telemetry rows, no metrics).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Hesamation/guvna/internal/config"
	"github.com/Hesamation/guvna/internal/metrics"
)

func TestProvidersProbeVerdicts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key string `json:"key"`
		}
		// The adaptor sets Bearer <key>; mirror it back in the response so
		// one upstream can serve every verdict class.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusForKey(r.Header.Get("Authorization")))
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		_ = body
	}))
	defer up.Close()

	providers := []config.Provider{{
		Name: "mock", Type: "openai", BaseURL: up.URL,
		KeyEnvs: []string{"KEY_OK", "KEY_DEAD", "KEY_LIMITED", "KEY_MISSING"},
		Models:  []string{"test-model"},
	}}
	_, ts := testServer(t, providers, nil, map[string]string{
		"KEY_OK":      "ok",
		"KEY_DEAD":    "dead",
		"KEY_LIMITED": "limited",
		// KEY_MISSING deliberately unset
	})

	resp := doAdminPost(t, ts, "/admin/providers/test", "")
	var out struct {
		Providers []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
			Keys  []struct {
				Env     string `json:"env"`
				Verdict string `json:"verdict"`
				Status  int    `json:"status"`
			} `json:"keys"`
		} `json:"providers"`
	}
	if err := decodeJSON(resp, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(out.Providers))
	}
	p := out.Providers[0]
	if p.Model != "test-model" {
		t.Errorf("model = %q, want test-model (first configured)", p.Model)
	}
	want := map[string]string{
		"KEY_OK":      "ok",
		"KEY_DEAD":    "dead",
		"KEY_LIMITED": "limited",
		"KEY_MISSING": "unset",
	}
	if len(p.Keys) != len(want) {
		t.Fatalf("probed %d keys, want %d", len(p.Keys), len(want))
	}
	for _, k := range p.Keys {
		if want[k.Env] != k.Verdict {
			t.Errorf("key %s verdict = %q, want %q", k.Env, k.Verdict, want[k.Env])
		}
	}
}

func TestProvidersProbeFilters(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	providers := []config.Provider{
		{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "KEY_A", Models: []string{"ma"}},
		{Name: "b", Type: "openai", BaseURL: up.URL, KeyEnv: "KEY_B", Models: []string{"mb"}},
	}
	_, ts := testServer(t, providers, nil, map[string]string{"KEY_A": "k", "KEY_B": "k"})

	resp := doAdminPost(t, ts, "/admin/providers/test", `{"provider":"b","model":"override-model"}`)
	var out struct {
		Providers []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"providers"`
	}
	if err := decodeJSON(resp, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Providers) != 1 || out.Providers[0].Name != "b" {
		t.Fatalf("providers = %+v, want only b", out.Providers)
	}
	if out.Providers[0].Model != "override-model" {
		t.Errorf("model = %q, want override-model", out.Providers[0].Model)
	}

	// Unknown provider filter is a 400.
	resp2 := doAdminPost(t, ts, "/admin/providers/test", `{"provider":"nope"}`)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown provider filter status = %d, want 400", resp2.StatusCode)
	}
}

func TestProvidersProbeIsSideEffectFree(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	providers := []config.Provider{{
		Name: "mock", Type: "openai", BaseURL: up.URL,
		KeyEnvs: []string{"KEY_A"}, Models: []string{"m"},
	}}
	s, ts := testServer(t, providers, nil, map[string]string{"KEY_A": "k"})

	// Quarantine the key first: a probe that ran through the pool would
	// MarkSuccess it, and the probe must not lie about the pool's health.
	if applied := s.pools["mock"].Mark("KEY_A", "auth"); applied == "" {
		t.Fatal("setup: quarantine did not apply")
	}

	doAdminPost(t, ts, "/admin/providers/test", "")
	if hits != 1 {
		t.Errorf("upstream hits = %d, want exactly one probe request", hits)
	}

	for _, ks := range s.pools["mock"].Keys() {
		if ks.State != "quarantined" || ks.Failures != 1 {
			t.Errorf("probe perturbed pool state: %+v", ks)
		}
	}
	for _, st := range s.hlth.Snapshot() {
		if st.Failures != 0 || st.Down {
			t.Errorf("probe marked step health: %+v", st)
		}
	}
	if err := s.tm.Flush(); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.tm.DB().QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("telemetry rows = %d, want 0 (probe must not write rows)", rows)
	}
	var buf strings.Builder
	if err := s.metrics.Write(&buf); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{metrics.StepAttempts, metrics.Quarantine} {
		if strings.Contains(buf.String(), "\n"+name+"{") {
			t.Errorf("metric %s recorded by probe:\n%s", name, buf.String())
		}
	}
}

// statusForKey lets one upstream serve every verdict class: the gateway
// forwards the Bearer key it picked, and the mock maps it to a status.
func statusForKey(authHeader string) int {
	switch {
	case strings.HasSuffix(authHeader, "dead"):
		return http.StatusForbidden
	case strings.HasSuffix(authHeader, "limited"):
		return http.StatusTooManyRequests
	default:
		return http.StatusOK
	}
}

func doAdminPost(t *testing.T, ts *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeJSON(resp *http.Response, out any) error {
	return json.NewDecoder(resp.Body).Decode(out)
}
