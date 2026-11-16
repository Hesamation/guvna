// Provider probes: the admin surface's one-command answer to "which keys are
// alive right now?" — the operational replacement for hand-curling each
// provider when a key 403s. A probe sends one minimal chat completion per key
// and classifies the verdict. Probes are pure diagnostics: they never touch
// key-pool state, step health, telemetry, or metrics.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Hesamation/guvna/internal/adaptors"
	"github.com/Hesamation/guvna/internal/config"
)

// probeTimeout bounds each key probe. Shorter than the relay's 5m header
// timeout on purpose: a diagnostic must not hang the admin request.
const probeTimeout = 10 * time.Second

// Verdicts are a fixed vocabulary — they may appear in dashboards/alerting
// later, so they must not drift casually.
const (
	VerdictOK       = "ok"          // 2xx
	VerdictDead     = "dead"        // 401/403: key rejected
	VerdictLimited  = "limited"     // 429
	VerdictUpstream = "upstream"    // 5xx
	VerdictUnreach  = "unreachable" // network error
	VerdictRejected = "rejected"    // other 4xx (bad model, bad request shape)
	VerdictUnset    = "unset"       // env var not set on the host
)

type keyProbe struct {
	Env       string `json:"env"`
	Verdict   string `json:"verdict"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail,omitempty"`
}

type providerProbe struct {
	Name  string     `json:"name"`
	Model string     `json:"model,omitempty"`
	Note  string     `json:"note,omitempty"` // provider-level reason no keys were probed
	Keys  []keyProbe `json:"keys"`
}

type probeRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type probeResponse struct {
	Providers []providerProbe `json:"providers"`
}

// handleProvidersTest probes every configured provider's keys (or one
// provider / one model when the optional body says so). An empty body means
// "probe everything". Admin-key only.
func (s *Server) handleProvidersTest(w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	if body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10)); err != nil {
		http.Error(w, `{"error":{"message":"read body"}}`, http.StatusBadRequest)
		return
	} else if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, `{"error":{"message":"invalid json"}}`, http.StatusBadRequest)
			return
		}
	}

	providers := make([]config.Provider, 0, len(s.cfg.Providers))
	if req.Provider != "" {
		p, ok := s.provider(req.Provider)
		if !ok {
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`,
				fmt.Sprintf("unknown provider %q", req.Provider)), http.StatusBadRequest)
			return
		}
		providers = append(providers, p)
	} else {
		providers = append(providers, s.cfg.Providers...)
	}

	// Providers run in parallel (one slow provider must not stretch the whole
	// probe); keys within a pool run sequentially, respecting provider rate
	// limits and mirroring how the pool itself serves.
	results := make([]providerProbe, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p config.Provider) {
			defer wg.Done()
			results[i] = s.probeProvider(r.Context(), p, req.Model)
		}(i, p)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(probeResponse{Providers: results})
}

// probeProvider probes one provider's keys sequentially and returns its
// result entry. It reads key envs from config (not pool state) so a quarantined
// key is still probed — the point is to re-test what the pool has set aside.
func (s *Server) probeProvider(ctx context.Context, p config.Provider, modelOverride string) providerProbe {
	res := providerProbe{Name: p.Name, Keys: make([]keyProbe, 0, len(p.AllKeyEnvs()))}
	model := modelOverride
	if model == "" && len(p.Models) > 0 {
		model = p.Models[0]
	}
	if model == "" {
		res.Note = "no probe model (add models: to config or pass one explicitly)"
		return res
	}
	res.Model = model
	for _, env := range p.AllKeyEnvs() {
		res.Keys = append(res.Keys, s.probeKey(ctx, p, env, model))
	}
	return res
}

// probeKey sends one minimal chat completion with a single key and classifies
// the response. It deliberately bypasses pools, health tracking, telemetry,
// and metrics — a probe must answer "is this key alive" without perturbing
// the state it reports on.
func (s *Server) probeKey(ctx context.Context, p config.Provider, env, model string) keyProbe {
	kp := keyProbe{Env: env}
	key := s.env(env)
	if key == "" {
		kp.Verdict = VerdictUnset
		kp.Detail = fmt.Sprintf("env %q not set", env)
		return kp
	}
	a, err := adaptors.New(p, key)
	if err != nil {
		kp.Verdict = VerdictRejected
		kp.Detail = err.Error()
		return kp
	}
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		kp.Verdict = VerdictRejected
		kp.Detail = err.Error()
		return kp
	}

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	resp, err := a.Chat(pctx, body)
	kp.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		if ctx.Err() != nil {
			kp.Verdict = VerdictUnreach
			kp.Detail = "admin request canceled"
			return kp
		}
		kp.Verdict = VerdictUnreach
		kp.Detail = err.Error()
		log.Printf("probe: %s/%s: %v", p.Name, env, err)
		return kp
	}
	defer resp.Body.Close()
	kp.Status = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		kp.Verdict = VerdictOK
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		kp.Verdict = VerdictDead
	case resp.StatusCode == 429:
		kp.Verdict = VerdictLimited
	case resp.StatusCode >= 500:
		kp.Verdict = VerdictUpstream
	default:
		kp.Verdict = VerdictRejected
	}
	return kp
}
