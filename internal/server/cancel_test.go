package server

// Client-cancel regression: a client disconnecting before the upstream
// answers must not quarantine keys, mark step health, or write telemetry
// rows — "client disconnect is not an upstream failure" (DECISIONS.md),
// extended to the pre-response phase.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Hesamation/guvna/internal/config"
	"github.com/Hesamation/guvna/internal/metrics"
)

func TestClientCancelRecordsNothing(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the upstream call open until the test is done
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	defer close(release)

	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "KEY_A"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "real-model"}}}}
	s, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"main","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-1")
	req = req.WithContext(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond) // let the upstream call go in flight
		cancel()
	}()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected client cancel error, got none")
	}

	time.Sleep(100 * time.Millisecond) // let the gateway abandon the request

	for _, ks := range s.pools["a"].Keys() {
		if ks.State != "healthy" || ks.Failures != 0 {
			t.Fatalf("key poisoned by client cancel: %+v", ks)
		}
	}
	for _, st := range s.hlth.Snapshot() {
		if st.Failures != 0 || st.Down {
			t.Fatalf("step health marked by client cancel: %+v", st)
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
		t.Fatalf("telemetry rows written for client cancel: %d", rows)
	}
	var buf strings.Builder
	if err := s.metrics.Write(&buf); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{metrics.ChatRequests, metrics.StepAttempts, metrics.Quarantine} {
		if strings.Contains(buf.String(), "\n"+name+"{") {
			t.Fatalf("metric %s recorded for client cancel:\n%s", name, buf.String())
		}
	}
}
