// Package server wires the HTTP surface: /healthz, /v1/models, and the
// /v1/chat/completions relay with chain-based fallback and SSE passthrough.
package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hesamation/guvna/internal/adaptors"
	"github.com/Hesamation/guvna/internal/auth"
	"github.com/Hesamation/guvna/internal/chains"
	"github.com/Hesamation/guvna/internal/config"
	"github.com/Hesamation/guvna/internal/health"
	"github.com/Hesamation/guvna/internal/keypool"
	"github.com/Hesamation/guvna/internal/logring"
	"github.com/Hesamation/guvna/internal/metrics"
	"github.com/Hesamation/guvna/internal/router"
	"github.com/Hesamation/guvna/internal/telemetry"
)

// KeepAliveInterval is how long the relay waits without upstream data before
// writing an SSE comment line to hold the connection open.
const KeepAliveInterval = 15 * time.Second

// retries is how many times a step is retried (beyond the first attempt) on
// transient failures (429/5xx/network) before the chain moves to the next step.
const retries = 2

// maxRequestBody caps inbound request bodies. Chat payloads — even with
// base64 images — stay far below this; without a cap, one giant body is
// held whole in memory by io.ReadAll and can OOM the gateway.
const maxRequestBody = 32 << 20

type Server struct {
	cfg       *config.Config
	rtr       *router.Router
	auth      *auth.Authenticator
	tm        *telemetry.Telemetry
	hlth      *health.Tracker
	logs      *logring.Ring
	store     *chains.Store
	pools     map[string]*keypool.Pool
	metrics   *metrics.Registry
	started   time.Time
	env       func(string) string
	retryWait func(attempt int) time.Duration
}

func New(cfg *config.Config, rtr *router.Router, a *auth.Authenticator, tm *telemetry.Telemetry, hlth *health.Tracker, logs *logring.Ring, store *chains.Store) *Server {
	pools := make(map[string]*keypool.Pool, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if pool, err := keypool.New(p.Name, p.AllKeyEnvs(), p.Rotation, p.Quarantine); err == nil {
			pools[p.Name] = pool
		}
	}
	s := &Server{cfg: cfg, rtr: rtr, auth: a, tm: tm, hlth: hlth, logs: logs, store: store, pools: pools, metrics: metrics.NewRegistry(), started: time.Now(), env: os.Getenv, retryWait: defaultRetryWait}
	s.metrics.RegisterGaugeFunc(metrics.KeypoolKeys, "Keys per provider by health state, sampled at scrape time.", s.sampleKeypoolKeys)
	s.metrics.RegisterGaugeFunc(metrics.StepDown, "1 when a chain step is in cool-off, 0 otherwise, sampled at scrape time.", s.sampleStepDown)
	s.metrics.RegisterGaugeFunc(metrics.StepFailures, "Consecutive failures per chain step, sampled at scrape time.", s.sampleStepFailures)
	s.metrics.RegisterGaugeFunc(metrics.Uptime, "Seconds since the gateway started.", func() []metrics.Sample {
		return []metrics.Sample{metrics.NewSample(time.Since(s.started).Seconds(), nil)}
	})
	return s
}

// sampleKeypoolKeys counts healthy vs quarantined keys per provider from
// live pool state. Providers without a pool emit no series.
func (s *Server) sampleKeypoolKeys() []metrics.Sample {
	var out []metrics.Sample
	for _, name := range s.poolNames() {
		healthy, quarantined := 0, 0
		for _, ks := range s.pools[name].Keys() {
			if ks.State == "quarantined" {
				quarantined++
			} else {
				healthy++
			}
		}
		out = append(out,
			metrics.NewSample(float64(healthy), metrics.Labels{"provider": name, "state": "healthy"}),
			metrics.NewSample(float64(quarantined), metrics.Labels{"provider": name, "state": "quarantined"}),
		)
	}
	return out
}

// sampleStepDown reports 1 for steps in cool-off, 0 for observed steps that
// are up. Steps never attempted emit no series.
func (s *Server) sampleStepDown() []metrics.Sample {
	var out []metrics.Sample
	for _, st := range s.hlth.Snapshot() {
		v := 0.0
		if st.Down {
			v = 1
		}
		out = append(out, metrics.NewSample(v, metrics.Labels{"provider": st.Provider, "model": st.Model}))
	}
	return out
}

func (s *Server) sampleStepFailures() []metrics.Sample {
	var out []metrics.Sample
	for _, st := range s.hlth.Snapshot() {
		out = append(out, metrics.NewSample(float64(st.Failures), metrics.Labels{"provider": st.Provider, "model": st.Model}))
	}
	return out
}

// defaultRetryWait backs off 250ms then 1s for the two retry attempts.
func defaultRetryWait(attempt int) time.Duration {
	if attempt <= 1 {
		return 250 * time.Millisecond
	}
	return time.Second
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /v1/models", s.auth.Middleware(http.HandlerFunc(s.handleModels)))
	mux.Handle("POST /v1/chat/completions", s.auth.Middleware(http.HandlerFunc(s.handleChat)))
	mux.Handle("GET /v1/chains", s.auth.Middleware(http.HandlerFunc(s.handleChainList)))
	mux.Handle("POST /v1/chains", s.auth.Middleware(http.HandlerFunc(s.handleChainCreate)))
	mux.Handle("DELETE /v1/chains/{name}", s.auth.Middleware(http.HandlerFunc(s.handleChainDelete)))
	mux.Handle("GET /admin/status", s.auth.AdminMiddleware(http.HandlerFunc(s.handleAdminStatus)))
	mux.Handle("GET /admin/logs", s.auth.AdminMiddleware(http.HandlerFunc(s.handleAdminLogs)))
	mux.Handle("GET /metrics", s.auth.AdminMiddleware(http.HandlerFunc(s.handleMetrics)))
	return mux
}

// handleMetrics renders Prometheus exposition. Admin-key only, like the
// rest of the admin surface; Prometheus scrapes it with a bearer token.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", `text/plain; version=0.0.4; charset=utf-8`)
	if err := s.metrics.Write(w); err != nil {
		log.Printf("metrics: write failed: %v", err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// handleAdminStatus reports chain health and usage aggregates to the admin
// (or a remote CLI with the admin key).
func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := s.tm.Stats()
	if err != nil {
		http.Error(w, `{"error":{"message":"stats unavailable"}}`, http.StatusInternalServerError)
		return
	}
	steps := s.hlth.Snapshot()
	stepsDown := 0
	for _, st := range steps {
		if st.Down {
			stepsDown++
		}
	}
	keys := make([]keypool.KeyStatus, 0, len(s.pools))
	for _, name := range s.poolNames() {
		keys = append(keys, s.pools[name].Keys()...)
	}
	resp := struct {
		Uptime    string              `json:"uptime"`
		Chains    []string            `json:"chains"`
		Steps     []health.StepStatus `json:"steps"`
		StepsDown int                 `json:"steps_down"`
		Keys      []keypool.KeyStatus `json:"keys"`
		Usage     telemetry.Stats     `json:"usage"`
	}{Uptime: time.Since(s.started).Round(time.Second).String()}
	resp.Chains = s.rtr.ChainNames()
	resp.Steps = steps
	resp.StepsDown = stepsDown
	resp.Keys = keys
	resp.Usage = stats
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	n := 100
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 512 {
			n = parsed
		}
	}
	resp := struct {
		Lines []string `json:"lines"`
	}{Lines: s.logs.Tail(n)}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	names := s.rtr.ChainNames()
	resp := struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}{Object: "list"}
	for _, n := range names {
		resp.Data = append(resp.Data, struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		}{ID: n, Object: "model", Created: time.Now().Unix(), OwnedBy: "guvna"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleChainList returns the current chains (config + runtime) with sources.
func (s *Server) handleChainList(w http.ResponseWriter, r *http.Request) {
	resp := struct {
		Chains []chainInfo `json:"chains"`
	}{}
	for _, c := range s.rtr.ListChains() {
		steps := make([]chainStep, 0, len(c.Steps))
		for _, st := range c.Steps {
			steps = append(steps, chainStep{Provider: st.Provider, Model: st.Model, Params: st.Params})
		}
		resp.Chains = append(resp.Chains, chainInfo{Name: c.Name, Source: c.Source, Steps: steps})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type chainStep struct {
	Provider string         `json:"provider"`
	Model    string         `json:"model"`
	Params   map[string]any `json:"params,omitempty"`
}

type chainInfo struct {
	Name   string      `json:"name"`
	Source string      `json:"source"`
	Steps  []chainStep `json:"steps"`
}

type chainCreateRequest struct {
	Name  string      `json:"name"`
	Steps []chainStep `json:"steps"`
}

// handleChainCreate adds a runtime chain and persists it. Any valid key
// (admin or client) may create chains — apps self-provision their routes.
func (s *Server) handleChainCreate(w http.ResponseWriter, r *http.Request) {
	var req chainCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"invalid json"}}`, http.StatusBadRequest)
		return
	}
	steps := make([]router.Step, 0, len(req.Steps))
	for _, st := range req.Steps {
		steps = append(steps, router.Step{Provider: st.Provider, Model: st.Model, Params: st.Params})
	}
	if err := s.rtr.AddChain(req.Name, steps); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), status)
		return
	}
	if s.store != nil {
		if err := s.store.Save(s.rtr); err != nil {
			log.Printf("chains: persist failed: %v", err)
			http.Error(w, `{"error":{"message":"chain added but not persisted"}}`, http.StatusInternalServerError)
			return
		}
	}
	log.Printf("chains: created %q (%d steps)", req.Name, len(steps))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(chainInfo{Name: req.Name, Source: "runtime", Steps: req.Steps})
}

// handleChainDelete removes a runtime chain. Config-defined chains are
// refused — they live in config.yaml.
func (s *Server) handleChainDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.rtr.RemoveChain(name); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), status)
		return
	}
	if s.store != nil {
		if err := s.store.Save(s.rtr); err != nil {
			log.Printf("chains: persist failed: %v", err)
			http.Error(w, `{"error":{"message":"chain removed but not persisted"}}`, http.StatusInternalServerError)
			return
		}
	}
	log.Printf("chains: deleted %q", name)
	w.WriteHeader(http.StatusNoContent)
}

type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		s.observeChat(unknownChain, metrics.OutcomeRejected, start)
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, `{"error":{"message":"request body too large"}}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":{"message":"read body"}}`, http.StatusBadRequest)
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.observeChat(unknownChain, metrics.OutcomeRejected, start)
		http.Error(w, `{"error":{"message":"invalid json"}}`, http.StatusBadRequest)
		return
	}
	chain, steps, err := s.rtr.Resolve(req.Model)
	if err != nil {
		s.observeChat(unknownChain, metrics.OutcomeRejected, start)
		if errors.Is(err, router.ErrNotFound) {
			http.Error(w, fmt.Sprintf(`{"error":{"message":"unknown chain or model %q — create a chain via POST /v1/chains or use a provider-prefixed model"}}`, req.Model), http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":{"message":"resolve failed"}}`, http.StatusInternalServerError)
		return
	}
	log.Printf("chat: chain=%s model=%q steps=%d stream=%v", chain, req.Model, len(steps), req.Stream)

	chainLabel := chain
	if chainLabel == "" {
		chainLabel = directChain // provider-prefixed passthrough has no chain name
	}
	var lastErr error
	var lastStatus int
	var lastBody []byte
	var lastCT string
	fellBack := false
	for i, step := range steps {
		if i > 0 {
			fellBack = true
		}
		if s.hlth.IsDown(step.Provider, step.Model) {
			log.Printf("chat: skipping %s/%s (cool-off)", step.Provider, step.Model)
			continue
		}
		resp, keyEnv, err := s.tryStep(r, step, body)
		if r.Context().Err() != nil {
			// Client went away mid-step: tryStep recorded nothing (no
			// quarantine, no health mark, no telemetry row) and there is
			// no connection left to relay to or report an outcome for.
			return
		}
		if err != nil {
			log.Printf("chat: step %s/%s failed: %v", step.Provider, step.Model, err)
			lastErr = err
			s.hlth.Mark(step.Provider, step.Model, 0, err)
			s.record(step, chain, req, 0, err, keyEnv)
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.hlth.MarkSuccess(step.Provider, step.Model)
			if p := s.pools[step.Provider]; p != nil {
				p.MarkSuccess(keyEnv)
			}
			// The upstream answered 2xx, but the relay itself can still
			// fail (truncated stream, mid-stream error) — the chat
			// outcome must reflect what the client actually got.
			failed := s.relaySuccess(w, r, step, chain, req, resp, keyEnv)
			outcome := metrics.OutcomeSuccess
			if failed {
				outcome = metrics.OutcomeFailed
			}
			s.observeChat(chainLabel, outcome, start)
			s.observeFallback(chainLabel, fellBack)
			return
		}
		lastStatus = resp.StatusCode
		lastBody, _ = io.ReadAll(resp.Body)
		lastCT = resp.Header.Get("Content-Type")
		resp.Body.Close()
		if retryable(resp.StatusCode, nil) || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// 401/403 means the step's keys are dead (tryStep already
			// quarantined them by class). Counting them toward step
			// health makes "all keys down → step down" real: after the
			// failure threshold the step is skipped instead of burning
			// the retry budget on every request. Other 4xx stay
			// unmarked — they are client/request errors.
			s.hlth.Mark(step.Provider, step.Model, resp.StatusCode, nil)
		}
		log.Printf("chat: step %s/%s failed: status %d", step.Provider, step.Model, resp.StatusCode)
		s.record(step, chain, req, resp.StatusCode, errors.New(http.StatusText(resp.StatusCode)), keyEnv)
	}
	// All steps failed: propagate the last upstream response as-is.
	log.Printf("chat: all steps failed for chain %q: %v", chain, lastErr)
	s.observeChat(chainLabel, metrics.OutcomeFailed, start)
	s.observeFallback(chainLabel, fellBack)
	if lastStatus == 0 {
		http.Error(w, `{"error":{"message":"no providers available"}}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", lastCT)
	w.WriteHeader(lastStatus)
	w.Write(lastBody)
}

// Chain labels for requests that never resolved to a named chain.
// "unknown" keeps rejected requests out of client-controlled cardinality:
// the requested model string is never used as a label value.
const (
	unknownChain = "unknown"
	directChain  = "direct"
)

// observeChat records one completed chat request: outcome counter plus
// end-to-end handler latency (including retries, fallbacks, and relay).
func (s *Server) observeChat(chain, outcome string, start time.Time) {
	s.metrics.IncCounter(metrics.ChatRequests, "Chat completion requests by chain and outcome.", 1, metrics.Labels{"chain": chain, "outcome": outcome})
	s.metrics.ObserveHistogram(metrics.ChatDuration, "End-to-end chat request latency in seconds, including retries and relay.", metrics.DefaultBuckets, time.Since(start).Seconds(), metrics.Labels{"chain": chain})
}

// observeFallback counts a request once when it attempted a step past the
// first — the first choice was unavailable or failed.
func (s *Server) observeFallback(chain string, fellBack bool) {
	if !fellBack {
		return
	}
	s.metrics.IncCounter(metrics.ChainFallbacks, "Chat requests that attempted a step past the first in their chain.", 1, metrics.Labels{"chain": chain})
}

// observeStep records one upstream attempt: outcome counter plus
// time-to-first-response latency.
func (s *Server) observeStep(step router.Step, outcome string, d time.Duration) {
	s.metrics.IncCounter(metrics.StepAttempts, "Upstream attempts by provider, model, and outcome.", 1, metrics.Labels{"provider": step.Provider, "model": step.Model, "outcome": outcome})
	s.metrics.ObserveHistogram(metrics.StepDuration, "Upstream attempt latency in seconds (time to first response).", metrics.DefaultBuckets, d.Seconds(), metrics.Labels{"provider": step.Provider, "model": step.Model})
}

// observeQuarantine counts a key quarantine application. Call only when
// Pool.Mark actually applied (non-empty return).
func (s *Server) observeQuarantine(provider string, class keypool.Class) {
	s.metrics.IncCounter(metrics.Quarantine, "Key quarantine applications by provider and failure class.", 1, metrics.Labels{"provider": provider, "class": string(class)})
}

// observeTokens counts usage tokens reported by the upstream. Zero counts
// create no series: usage reporting is best-effort.
func (s *Server) observeTokens(step router.Step, in, out int64) {
	if in > 0 {
		s.metrics.IncCounter(metrics.Tokens, "Tokens reported by upstreams, by direction.", float64(in), metrics.Labels{"provider": step.Provider, "model": step.Model, "direction": "in"})
	}
	if out > 0 {
		s.metrics.IncCounter(metrics.Tokens, "Tokens reported by upstreams, by direction.", float64(out), metrics.Labels{"provider": step.Provider, "model": step.Model, "direction": "out"})
	}
}

// outcomeForStatus maps an upstream HTTP status to a step-attempt outcome.
func outcomeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return metrics.OutcomeAuthError
	case status == http.StatusTooManyRequests:
		return metrics.OutcomeRateLimited
	case status >= 500:
		return metrics.OutcomeServerError
	default:
		return metrics.OutcomeClientError
	}
}

// tryStep attempts one chain step, retrying transient failures (429, 5xx,
// network errors) up to retries times with backoff. Each attempt picks a key
// from the provider's pool; quarantinable failures rotate to the next key.
// Streaming is safe: a retry only happens when no 2xx response has been
// received yet — once the upstream starts streaming we are committed to that
// stream.
//
// A dead request context (client disconnect) is not an upstream failure: no
// key is quarantined, no step health mark is taken, and no attempt is
// observed — the caller checks the context and abandons the request.
//
// Returns (resp, keyEnv, nil) when an upstream response was obtained (2xx or
// not — body left open for the caller to relay), or (nil, "", err) when every
// attempt failed at the network level.
func (s *Server) tryStep(r *http.Request, step router.Step, body []byte) (*http.Response, string, error) {
	p, ok := s.provider(step.Provider)
	if !ok {
		return nil, "", fmt.Errorf("provider %q not in config", step.Provider)
	}
	pool := s.pools[step.Provider]
	if pool == nil {
		// Pools are built for every configured provider in New; a missing
		// pool means config and server disagree, which must surface as an
		// error, not a lazily created pool racing concurrent requests.
		return nil, "", fmt.Errorf("provider %q has no key pool", step.Provider)
	}
	payload := rewritePayload(body, step.Model, step.Params)
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			if r.Context().Err() != nil {
				return nil, "", r.Context().Err()
			}
			if s.retryWait != nil {
				time.Sleep(s.retryWait(attempt))
			}
		}
		keyEnv := pool.Pick()
		key := s.env(keyEnv)
		if key == "" {
			err := fmt.Errorf("provider %q: key env %q not set", p.Name, keyEnv)
			log.Printf("chat: %s/%s attempt %d: %v", step.Provider, step.Model, attempt+1, err)
			lastErr = err
			continue
		}
		a, err := adaptors.New(p, key)
		if err != nil {
			log.Printf("chat: %s/%s attempt %d: %v", step.Provider, step.Model, attempt+1, err)
			lastErr = err
			continue
		}
		attemptStart := time.Now()
		resp, err := a.Chat(r.Context(), payload)
		attemptTook := time.Since(attemptStart)
		if err != nil {
			if r.Context().Err() != nil {
				// The client disconnected while the attempt was in flight:
				// not an upstream failure, not the key's fault. Record
				// nothing (DECISIONS 2026-08-16, extended to the
				// pre-response phase 2026-11-16).
				return nil, "", err
			}
			log.Printf("chat: %s/%s attempt %d: %v", step.Provider, step.Model, attempt+1, err)
			s.observeStep(step, metrics.OutcomeNetworkErr, attemptTook)
			if applied := pool.Mark(keyEnv, keypool.ClassFor(0, err)); applied != "" {
				s.observeQuarantine(step.Provider, applied)
			}
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.observeStep(step, metrics.OutcomeSuccess, attemptTook)
			return resp, keyEnv, nil
		}
		log.Printf("chat: %s/%s attempt %d: status %d", step.Provider, step.Model, attempt+1, resp.StatusCode)
		s.observeStep(step, outcomeForStatus(resp.StatusCode), attemptTook)
		if !retryable(resp.StatusCode, nil) && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			return resp, keyEnv, nil // other 4xx: config/request error, never retried
		}
		if applied := pool.Mark(keyEnv, keypool.ClassFor(resp.StatusCode, nil)); applied != "" {
			s.observeQuarantine(step.Provider, applied)
		}
		if attempt == retries {
			return resp, keyEnv, nil // transient failures exhausted: last response
		}
		resp.Body.Close()
	}
	return nil, "", lastErr
}

// retryable reports whether a failure is transient: network errors, rate
// limits, and 5xx. Client errors (4xx) are config or request problems and are
// never retried or counted against provider health.
func retryable(status int, err error) bool {
	if err != nil {
		return true
	}
	return status == http.StatusTooManyRequests || status >= 500
}

// rewritePayload unmarshals the JSON body exactly once to apply both the "model"
// override and any step-level params (as fill-missing defaults). Explicit client
// values for params always win.
func rewritePayload(body []byte, model string, params map[string]any) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}

	modified := false
	if m["model"] != nil {
		enc, err := json.Marshal(model)
		if err == nil {
			m["model"] = enc
			modified = true
		}
	}

	if len(params) > 0 {
		for k, v := range params {
			if _, present := m[k]; present {
				continue
			}
			enc, err := json.Marshal(v)
			if err != nil {
				continue
			}
			m[k] = enc
			modified = true
		}
	}

	if !modified {
		return body
	}

	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func (s *Server) provider(name string) (config.Provider, bool) {
	for _, p := range s.cfg.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return config.Provider{}, false
}

// poolNames returns provider names that have a key pool, sorted.
func (s *Server) poolNames() []string {
	names := make([]string, 0, len(s.pools))
	for n := range s.pools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// relaySuccess relays a 2xx upstream response to the client. It reports
// whether the relay failed to deliver a complete response: a truncated
// stream or a mid-stream failure counts as failed even though the
// upstream answered 2xx, so the chat-level outcome metric stays honest.
// A client disconnect is not a relay failure.
func (s *Server) relaySuccess(w http.ResponseWriter, r *http.Request, step router.Step, chain string, req chatRequest, resp *http.Response, keyEnv string) bool {
	defer resp.Body.Close()
	if !req.Stream {
		// Non-streaming: relay body, best-effort token count.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if r.Context().Err() != nil {
				// Client disconnected — not an upstream failure.
				return false
			}
			s.hlth.Mark(step.Provider, step.Model, 0, err)
			s.poolRecord(keyEnv, step, chain, req, 502, 0, 0, keypool.ClassTransient, err.Error())
			http.Error(w, `{"error":{"message":"upstream read failed"}}`, http.StatusBadGateway)
			return true
		}
		in, out := telemetry.ParseUsage(body)
		s.poolRecord(keyEnv, step, chain, req, resp.StatusCode, in, out, "", "")
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.Header().Set("X-Guvna-Step", step.Provider+"/"+step.Model)
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return false
	}
	return s.relayStream(w, r, step, chain, req, resp, keyEnv)
}

func (s *Server) relayStream(w http.ResponseWriter, r *http.Request, step router.Step, chain string, req chatRequest, resp *http.Response, keyEnv string) bool {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":{"message":"streaming unsupported"}}`, http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Guvna-Step", step.Provider+"/"+step.Model)
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	br := bufio.NewReader(resp.Body)
	var in, out int64
	var sawData, sawDone bool

	// Keep-alive writer: the relay loop below blocks in ReadBytes during
	// upstream silence, so a timer check in the loop can only run after
	// data arrives — exactly when a keep-alive is not needed. A dedicated
	// goroutine writes an SSE comment after KeepAliveInterval without a
	// relayed line; the loop reports each relayed line on activity so the
	// timer tracks real write activity. All writes to w are serialized
	// under mu: http.ResponseWriter is not safe for concurrent use.
	var mu sync.Mutex
	activity := make(chan struct{}, 1)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		timer := time.NewTimer(KeepAliveInterval)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-r.Context().Done():
				return
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(KeepAliveInterval)
			case <-timer.C:
				mu.Lock()
				_, err := io.WriteString(w, ": keep-alive\n\n")
				if err == nil {
					flusher.Flush()
				}
				mu.Unlock()
				if err != nil {
					return // client gone; the relay loop will notice too
				}
				timer.Reset(KeepAliveInterval)
			}
		}
	}()
	defer func() { close(done); <-stopped }()

	for {
		select {
		case <-r.Context().Done():
			return false
		default:
		}
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			mu.Lock()
			_, werr := w.Write(line)
			if werr == nil {
				flusher.Flush()
			}
			mu.Unlock()
			if werr != nil {
				return false
			}
			select {
			case activity <- struct{}{}:
			default:
			}
			// Streaming usage arrives in the final data: chunk; capture it
			// best-effort for telemetry.
			if payload := dataPayload(line); payload != nil {
				sawData = true
				if string(payload) == "[DONE]" {
					sawDone = true
				}
				if i, o := telemetry.ParseUsage(payload); i != 0 || o != 0 {
					in, out = i, o
				}
			}
		}
		if err != nil {
			if r.Context().Err() != nil {
				return false // client disconnected mid-stream
			}
			if errors.Is(err, io.EOF) {
				if sawData && !sawDone {
					// Truncated stream: upstream closed cleanly after
					// partial data without the terminal [DONE]. Without
					// this the client mistakes a partial response for a
					// complete one and the step is recorded as a success.
					// Surface it like any other mid-stream failure.
					log.Printf("chat: stream truncated from %q (no [DONE])", step.Provider)
					mu.Lock()
					io.WriteString(w, `data: {"error":{"message":"upstream stream truncated"}}`+"\n\n")
					flusher.Flush()
					mu.Unlock()
					s.hlth.Mark(step.Provider, step.Model, 0, errors.New("stream truncated (no [DONE])"))
					s.poolRecord(keyEnv, step, chain, req, 502, in, out, keypool.ClassTransient, "stream truncated (no [DONE])")
					return true
				}
				s.hlth.MarkSuccess(step.Provider, step.Model)
				if p := s.pools[step.Provider]; p != nil {
					p.MarkSuccess(keyEnv)
				}
				s.poolRecord(keyEnv, step, chain, req, resp.StatusCode, in, out, "", "")
				return false
			}
			// Mid-stream failure: propagate as an error chunk, then close.
			// Mark the provider so later requests skip it during cool-off.
			log.Printf("chat: stream error from %q: %v", step.Provider, err)
			mu.Lock()
			io.WriteString(w, `data: {"error":{"message":"upstream stream error"}}`+"\n\n")
			flusher.Flush()
			mu.Unlock()
			s.hlth.Mark(step.Provider, step.Model, 0, err)
			s.poolRecord(keyEnv, step, chain, req, 502, in, out, keypool.ClassTransient, err.Error())
			return true
		}
	}
}

// poolRecord records one completed step attempt against the serving key:
// pool state (failures for class != "", success otherwise), usage counters,
// and exactly one telemetry row — status, tokens, and err text together, so
// an attempt is never split across two rows.
func (s *Server) poolRecord(keyEnv string, step router.Step, chain string, req chatRequest, status int, in, out int64, class keypool.Class, errStr string) {
	p := s.pools[step.Provider]
	if p == nil || keyEnv == "" {
		return
	}
	if class != "" {
		if applied := p.Mark(keyEnv, class); applied != "" {
			s.observeQuarantine(step.Provider, applied)
		}
	} else {
		p.MarkSuccess(keyEnv)
	}
	p.Record(keyEnv, in, out)
	s.observeTokens(step, in, out)
	s.tm.Record(telemetry.Event{
		Provider:  step.Provider,
		Chain:     chain,
		Model:     step.Model,
		Stream:    req.Stream,
		Status:    status,
		TokensIn:  in,
		TokensOut: out,
		Key:       keyEnv,
		Err:       errStr,
		Ts:        time.Now(),
	})
}

// dataPayload returns the JSON payload of an SSE data: line, or nil.
func dataPayload(line []byte) []byte {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return nil
	}
	return []byte(strings.TrimSpace(s[len("data:"):]))
}

// record writes one telemetry row for an attempt that failed before any
// tokens existed (network error, non-2xx). Completed attempts with a body go
// through poolRecord, which writes status, tokens, and err in one row.
func (s *Server) record(step router.Step, chain string, req chatRequest, status int, err error, key string) {
	e := telemetry.Event{
		Provider: step.Provider,
		Chain:    chain,
		Model:    step.Model,
		Stream:   req.Stream,
		Status:   status,
		Key:      key,
		Ts:       time.Now(),
	}
	if err != nil {
		e.Err = err.Error()
	}
	s.tm.Record(e)
}
