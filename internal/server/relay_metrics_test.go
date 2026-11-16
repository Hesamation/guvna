package server

// Truncated-stream metric test: an upstream that closes a stream without
// the terminal [DONE] must count the chat request as outcome="failed",
// not "success", even though the upstream answered 2xx.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Hesamation/guvna/internal/config"
	"github.com/Hesamation/guvna/internal/metrics"
)

func TestTruncatedStreamCountsFailed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Partial data, then a clean close with no [DONE].
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	}))
	defer up.Close()

	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "KEY_A"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "real-model"}}}}
	s, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k"})

	resp := doChat(t, ts, "main", true)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "upstream stream truncated") {
		t.Fatalf("expected truncation error chunk, got %s", body)
	}

	var buf bytes.Buffer
	if err := s.metrics.Write(&buf); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, metrics.ChatRequests+"{") {
			continue
		}
		if strings.Contains(line, `outcome="failed"`) {
			return
		}
		if strings.Contains(line, `outcome="success"`) {
			t.Fatalf("truncated stream counted as success: %s", line)
		}
	}
	t.Fatalf("no %s series in exposition:\n%s", metrics.ChatRequests, buf.String())
}
