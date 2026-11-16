package adaptors

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Hesamation/guvna/internal/config"
)

func testUpstream(t *testing.T, wantPath, wantKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			http.Error(w, "bad path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			http.Error(w, "bad content type", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"echo":` + string(body) + `}`))
	}))
}

func TestOpenAICompatForwards(t *testing.T) {
	up := testUpstream(t, "/v1/chat/completions", "sk-test")
	defer up.Close()

	a := NewOpenAICompat(config.Provider{Name: "groq", BaseURL: up.URL}, "sk-test")
	resp, err := a.Chat(context.Background(), []byte(`{"model":"llama-3.3-70b-versatile"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "llama-3.3-70b-versatile") {
		t.Errorf("body not forwarded: %s", got)
	}
}

func TestGeminiUsesOpenAICompatEndpoint(t *testing.T) {
	up := testUpstream(t, "/v1beta/openai/v1/chat/completions", "gk-test")
	defer up.Close()

	a := NewGemini(config.Provider{Name: "gemini", BaseURL: up.URL}, "gk-test")
	resp, err := a.Chat(context.Background(), []byte(`{"model":"gemini-2.5-flash"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestNewByType(t *testing.T) {
	if _, err := New(config.Provider{Name: "x", Type: "openai", BaseURL: "http://x"}, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.Provider{Name: "x", Type: "gemini", BaseURL: "http://x"}, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.Provider{Name: "x", Type: "anthropic", BaseURL: "http://x"}, "k"); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestClientHasResponseHeaderTimeout(t *testing.T) {
	tr, ok := Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Client.Transport = %T", Client.Transport)
	}
	if tr.ResponseHeaderTimeout != ResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, ResponseHeaderTimeout)
	}
}

func TestHeaderTimeoutBoundsSilentUpstream(t *testing.T) {
	// Upstream accepts the connection and never writes a response. The
	// handler waits on release (not r.Context().Done(): a client-side
	// header timeout does not reliably cancel the server-side request
	// context, which would wedge httptest Close).
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() { close(release); up.Close() }()
	client := &http.Client{Transport: preferV4Transport(100 * time.Millisecond)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, up.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	start := time.Now()
	_, err := client.Do(req)
	if err == nil {
		t.Fatal("expected timeout error from silent upstream")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("silent upstream held the request for %v, want header-timeout failure", elapsed)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %v, want a timeout error", err)
	}
}

func TestGeminiFailsWithoutKey(t *testing.T) {
	up := testUpstream(t, "/v1beta/openai/v1/chat/completions", "gk-test")
	defer up.Close()
	a := NewGemini(config.Provider{Name: "gemini", BaseURL: up.URL}, "")
	resp, err := a.Chat(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
