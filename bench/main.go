// bench is a standalone load harness for OpenAI-compatible gateways. It is
// deliberately outside the gateway binary (the leanest possible core does not
// grow a load tester): build it with `go build -o /tmp/bench ./bench`.
//
// Two subcommands:
//
//	bench mock -addr 127.0.0.1:9000
//	  Serve a zero-latency OpenAI-compatible upstream (streaming and
//	  non-streaming, with usage in the final chunk). Point any gateway's
//	  provider config at it to measure gateway overhead, not provider latency.
//
//	bench load -url http://127.0.0.1:20128 -key KEY -model m \
//	           -conns 8 -dur 30s -stream=false -rss-pid PID
//	  Drive chat completions until -dur elapses; report RPS and p50/p95/p99/max
//	  latency. -rss-pid samples a process's VmRSS from /proc every 250ms and
//	  reports idle (first 2s), peak, and end-of-run values.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "mock":
		mockCmd(os.Args[2:])
	case "load":
		loadCmd(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage:\n  bench mock -addr 127.0.0.1:9000\n  bench load -url URL -key KEY -model M [-conns N] [-dur 30s] [-stream] [-rss-pid PID]\n")
}

// --- mock upstream ---

func mockCmd(args []string) {
	fs := flag.NewFlagSet("mock", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9000", "listen address")
	fs.Parse(args)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleMockChat)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	fmt.Printf("mock upstream on http://%s (openai-compatible, zero latency)\n", *addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// handleMockChat answers any POST with a minimal completion (the path suffix
// differs per gateway: /v1/chat/completions, /chat/completions, ...).
func handleMockChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream bool `json:"stream"`
	}
	body, _ := io.ReadAll(r.Body)
	json.Unmarshal(body, &req)

	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	fl.Flush()
	w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
	fl.Flush()
	w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n"))
	fl.Flush()
	w.Write([]byte("data: [DONE]\n\n"))
	fl.Flush()
}

// --- load driver ---

func loadCmd(args []string) {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:20128", "gateway base URL")
	key := fs.String("key", "", "Bearer key")
	model := fs.String("model", "bench", "model name to request")
	conns := fs.Int("conns", 8, "concurrent workers")
	dur := fs.Duration("dur", 30*time.Second, "run duration")
	stream := fs.Bool("stream", false, "streaming requests")
	timeout := fs.Duration("timeout", 15*time.Second, "per-request timeout")
	path := fs.String("path", "/v1/chat/completions", "chat endpoint path")
	rssPid := fs.Int("rss-pid", 0, "sample VmRSS of this pid (0 = off)")
	fs.Parse(args)

	if *rssPid > 0 {
		idle, peak := sampleRSS(*rssPid, 2*time.Second)
		fmt.Printf("rss idle=%dKB peak-before-load=%dKB\n", idle, peak)
		peakStart = peak
	}

	body, _ := json.Marshal(map[string]any{
		"model":      *model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     *stream,
	})

	client := &http.Client{Timeout: *timeout}
	var completed, failed atomic.Int64
	var latMu sync.Mutex
	var latencies []time.Duration

	deadline := time.Now().Add(*dur)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < *conns; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				reqStart := time.Now()
				err := oneRequest(client, *url+*path, *key, body)
				d := time.Since(reqStart)
				latMu.Lock()
				latencies = append(latencies, d)
				latMu.Unlock()
				if err != nil {
					failed.Add(1)
				} else {
					completed.Add(1)
				}
			}
		}()
	}

	var rssPeak atomic.Int64
	if *rssPid > 0 {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
					if v := readRSS(*rssPid); v > rssPeak.Load() {
						rssPeak.Store(v)
					}
					time.Sleep(250 * time.Millisecond)
				}
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	quant := func(q float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		i := int(float64(len(latencies)-1) * q)
		return latencies[i]
	}
	phase := "nonstream"
	if *stream {
		phase = "stream"
	}
	fmt.Printf("phase=%s conns=%d dur=%v requests=%d failed=%d rps=%.0f p50=%v p95=%v p99=%v max=%v",
		phase, *conns, elapsed.Round(time.Second), completed.Load(), failed.Load(),
		float64(completed.Load())/elapsed.Seconds(),
		quant(0.5).Round(time.Millisecond), quant(0.95).Round(time.Millisecond),
		quant(0.99).Round(time.Millisecond), latencies[len(latencies)-1].Round(time.Millisecond))
	if *rssPid > 0 {
		fmt.Printf(" rss-end=%dKB rss-peak=%dKB", readRSS(*rssPid), max64(rssPeak.Load(), peakStart))
	}
	fmt.Println()
}

var peakStart int64

func oneRequest(client *http.Client, url, key string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// sampleRSS reads VmRSS every 250ms for d and returns (min, max) — the window
// doubles as the "idle" measurement right after startup.
func sampleRSS(pid int, d time.Duration) (int64, int64) {
	deadline := time.Now().Add(d)
	var mn, mx int64
	for time.Now().Before(deadline) {
		v := readRSS(pid)
		if mn == 0 || v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
		time.Sleep(250 * time.Millisecond)
	}
	return mn, mx
}

// readRSS returns VmRSS in KB from /proc, 0 when unreadable.
func readRSS(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, _ := strconv.ParseInt(fields[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
