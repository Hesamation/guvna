// Tool-calling relay tests: streaming tool-call deltas, tool_choice
// "auto" passthrough, multi-tool payloads, and multi-turn loops through
// the in-process chat handler with scripted mock upstreams. All tests
// run offline on every `go test` invocation; assertions are plumbing
// only (status, SSE shape, [DONE] termination, tool-call structure),
// never model quality.
//
// Live counterparts: the same four paths against the Kilo anonymous
// lane live in the standalone guvnaharness module (outside this repo),
// driving the compiled binary over HTTP. Run them from that module
// with:
//
//	GUVNA_LIVE=1 go test -run TestLive -v -timeout 10m .
//
// They are intentionally not ported here: they need network access
// and cannot run in CI. The mock tests below are the CI-safe subset.
package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hesamation/guvna/internal/config"
)

// --- OpenAI wire shapes (subset used by the tests) ---

type chatMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

func strptr(s string) *string { return &s }

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func funcTool(name, desc string, props map[string]any, required []string) toolDef {
	var td toolDef
	td.Type = "function"
	td.Function.Name = name
	td.Function.Description = desc
	td.Function.Parameters = map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
	return td
}

func weatherTool() toolDef {
	return funcTool("get_weather", "Get the weather for a city",
		map[string]any{"city": map[string]any{"type": "string"}}, []string{"city"})
}

func timeTool() toolDef {
	return funcTool("get_time", "Get the current time in a timezone",
		map[string]any{"timezone": map[string]any{"type": "string"}}, []string{"timezone"})
}

func calcTool() toolDef {
	return funcTool("calculate", "Evaluate an arithmetic expression",
		map[string]any{"expression": map[string]any{"type": "string"}}, []string{"expression"})
}

type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Role      string `json:"role,omitempty"`
			Content   string `json:"content,omitempty"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

// recordingUpstream is a scripted OpenAI-compatible upstream. It records
// every request body and asserts the guvna relay contract: path
// /v1/chat/completions, Bearer auth, model rewritten to the step model.
type recordingUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newRecordingUpstream(t *testing.T, handle func(t *testing.T, w http.ResponseWriter, r *http.Request, body []byte, n int)) *recordingUpstream {
	t.Helper()
	m := &recordingUpstream{}
	var n int
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
			http.Error(w, `{"error":"wrong path"}`, http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("upstream Authorization = %q, want Bearer k", got)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Errorf("upstream body is not JSON: %v", err)
			http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
			return
		}
		if probe.Model != "m-a" {
			t.Errorf("upstream model = %q, want m-a (chain name must be rewritten)", probe.Model)
			http.Error(w, `{"error":"wrong model"}`, http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.bodies = append(m.bodies, raw)
		n++
		call := n
		m.mu.Unlock()
		handle(t, w, r, raw, call)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *recordingUpstream) requests() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.bodies))
	copy(out, m.bodies)
	return out
}

// toolTestServer builds a one-step chain (provider a, model m-a) pointed
// at the recording upstream.
func toolTestServer(t *testing.T, m *recordingUpstream) *httptest.Server {
	t.Helper()
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: m.srv.URL, KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m-a"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"K": "k"})
	return ts
}

// postChatPayload sends one chat completion with the given payload and
// returns the raw response. The caller owns closing resp.Body.
func postChatPayload(t *testing.T, ts *httptest.Server, payload map[string]any) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// readChatResponse asserts a 200 JSON response and decodes it.
func readChatResponse(t *testing.T, resp *http.Response) chatResponse {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("chat status = %d, want 200; body = %s", resp.StatusCode, raw)
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding chat response: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	return out
}

// assertGuvnaStep asserts the response passed through guvna's relay.
func assertGuvnaStep(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	if got := resp.Header.Get("X-Guvna-Step"); got != want {
		t.Errorf("X-Guvna-Step = %q, want %q", got, want)
	}
}

// sseEvent is one parsed SSE data: event with its arrival time.
type sseEvent struct {
	data string
	at   time.Time
}

// readSSE reads a streaming response to completion, returning the data:
// events in arrival order and whether the [DONE] terminator was seen.
// Comment lines (": keep-alive") are skipped.
func readSSE(t *testing.T, resp *http.Response) ([]sseEvent, bool) {
	t.Helper()
	defer resp.Body.Close()
	var events []sseEvent
	done := false
	br := bufio.NewReader(resp.Body)
	var dataLines []string
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if data == "[DONE]" {
			done = true
			return
		}
		events = append(events, sseEvent{data: data, at: time.Now()})
	}
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(trimmed[len("data:"):]))
		case trimmed == "":
			flush()
		case strings.HasPrefix(trimmed, ":"):
			// SSE comment (guvna keep-alive); ignore.
		}
		if err != nil {
			flush()
			if err != io.EOF {
				t.Fatalf("reading SSE stream: %v", err)
			}
			return events, done
		}
	}
}

// reassembleToolCalls merges chunked tool_call deltas (name and arguments
// arrive split across chunks) into complete calls, keyed by index.
func reassembleToolCalls(t *testing.T, events []sseEvent) ([]toolCall, string) {
	t.Helper()
	var calls []toolCall
	var finishReason string
	for _, ev := range events {
		var chunk streamChunk
		if err := json.Unmarshal([]byte(ev.data), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v: %s", err, ev.data)
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != "" {
				finishReason = ch.FinishReason
			}
			for _, dc := range ch.Delta.ToolCalls {
				idx := dc.Index
				for len(calls) <= idx {
					calls = append(calls, toolCall{})
				}
				if dc.ID != "" {
					calls[idx].ID = dc.ID
				}
				if dc.Type != "" {
					calls[idx].Type = dc.Type
				}
				calls[idx].Function.Name += dc.Function.Name
				calls[idx].Function.Arguments += dc.Function.Arguments
			}
		}
	}
	return calls, finishReason
}

// assertValidToolCall checks the plumbing shape of a tool call: type, id,
// name, and JSON-parseable arguments. It says nothing about whether the
// arguments are sensible — that is model quality, out of scope.
func assertValidToolCall(t *testing.T, tc toolCall) map[string]any {
	t.Helper()
	if tc.Type != "function" {
		t.Errorf("tool call type = %q, want function", tc.Type)
	}
	if tc.ID == "" {
		t.Error("tool call has empty id")
	}
	if tc.Function.Name == "" {
		t.Error("tool call has empty function name")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("tool call arguments are not JSON: %v: %q", err, tc.Function.Arguments)
	}
	return args
}

// bodyJSON decodes a recorded upstream request body into a generic map.
func bodyJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("upstream body is not JSON: %v: %s", err, raw)
	}
	return m
}

func toolNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	tools, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("upstream body has no tools array: %v", body)
	}
	var names []string
	for _, tl := range tools {
		fn, ok := tl.(map[string]any)["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool entry has no function object: %v", tl)
		}
		names = append(names, fn["name"].(string))
	}
	return names
}

func forcedChoice() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}
}

// TestChatStreamingToolCallDeltas: a streaming tool call whose name and
// arguments arrive split across SSE chunks must pass through guvna
// incrementally (first chunk delivered before the upstream finishes) and
// reassemble into a complete, valid tool call.
func TestChatStreamingToolCallDeltas(t *testing.T) {
	m := newRecordingUpstream(t, func(t *testing.T, w http.ResponseWriter, r *http.Request, body []byte, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter does not flush")
		}
		write := func(s string) {
			io.WriteString(w, s)
			flusher.Flush()
		}
		write("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		// Pause: the client must see chunk 1 while the upstream is still
		// working, proving guvna relays incrementally instead of buffering.
		time.Sleep(400 * time.Millisecond)
		write("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_wea\"}}]}}]}\n\n")
		write("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"ther\",\"arguments\":\"{\\\"city\\\": \\\"Ra\"}}]}}]}\n\n")
		write("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"sht\\\"}\"}}]}}]}\n\n")
		write("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n")
		write("data: [DONE]\n\n")
	})
	ts := toolTestServer(t, m)

	payload := map[string]any{
		"model":       "main",
		"stream":      true,
		"messages":    []any{map[string]any{"role": "user", "content": "What is the weather in Rasht?"}},
		"tools":       []any{weatherTool()},
		"tool_choice": forcedChoice(),
		"max_tokens":  64,
	}
	resp := postChatPayload(t, ts, payload)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("chat status = %d, want 200; body = %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	assertGuvnaStep(t, resp, "a/m-a")

	events, done := readSSE(t, resp)
	if !done {
		t.Error("stream ended without [DONE]")
	}
	if len(events) < 4 {
		t.Fatalf("data events = %d, want >= 4 (role + 3 tool deltas + finish)", len(events))
	}
	// Incremental delivery: the first event must have arrived >=300ms
	// before the last, matching the upstream's 400ms pause after chunk 1.
	// A relay that buffered the whole stream would deliver all events at
	// once after the handler finished.
	first, last := events[0].at, events[len(events)-1].at
	if last.Sub(first) < 300*time.Millisecond {
		t.Errorf("first-to-last event span = %v, want >= 300ms (upstream paused 400ms after chunk 1)", last.Sub(first))
	}

	calls, finishReason := reassembleToolCalls(t, events)
	if finishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finishReason)
	}
	if len(calls) != 1 {
		t.Fatalf("reassembled tool calls = %d, want 1", len(calls))
	}
	args := assertValidToolCall(t, calls[0])
	if calls[0].Function.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", calls[0].Function.Name)
	}
	if args["city"] != "Rasht" {
		t.Errorf("tool args = %v, want city=Rasht", args)
	}

	// Passthrough: the upstream must have received stream=true, the tools
	// array, and the forced tool_choice untouched.
	reqs := m.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
	upBody := bodyJSON(t, reqs[0])
	if upBody["stream"] != true {
		t.Errorf("upstream stream = %v, want true", upBody["stream"])
	}
	if got := toolNames(t, upBody); len(got) != 1 || got[0] != "get_weather" {
		t.Errorf("upstream tools = %v, want [get_weather]", got)
	}
}

// TestChatToolChoiceAutoPassthrough: tool_choice "auto" and the tools array
// must reach the upstream untouched, and a content response (model
// discretion) must relay as a valid completion.
func TestChatToolChoiceAutoPassthrough(t *testing.T) {
	m := newRecordingUpstream(t, func(t *testing.T, w http.ResponseWriter, r *http.Request, body []byte, n int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"cmpl-auto","model":"m-a","choices":[{"message":{"role":"assistant","content":"Sunny in Rasht."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	})
	ts := toolTestServer(t, m)

	payload := map[string]any{
		"model":       "main",
		"messages":    []any{map[string]any{"role": "user", "content": "What is the weather in Rasht?"}},
		"tools":       []any{weatherTool()},
		"tool_choice": "auto",
		"max_tokens":  64,
	}
	resp := postChatPayload(t, ts, payload)
	assertGuvnaStep(t, resp, "a/m-a")
	out := readChatResponse(t, resp)
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", out.Choices[0].FinishReason)
	}
	if out.Choices[0].Message.Content == nil || *out.Choices[0].Message.Content == "" {
		t.Error("expected non-empty content for a stop response")
	}

	reqs := m.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
	upBody := bodyJSON(t, reqs[0])
	if upBody["tool_choice"] != "auto" {
		t.Errorf("upstream tool_choice = %v, want \"auto\"", upBody["tool_choice"])
	}
	if got := toolNames(t, upBody); len(got) != 1 || got[0] != "get_weather" {
		t.Errorf("upstream tools = %v, want [get_weather]", got)
	}
}

// TestChatMultiToolForcedChoice: a three-tool payload with a forced choice
// must reach the upstream whole, and the forced function's tool call must
// relay back intact.
func TestChatMultiToolForcedChoice(t *testing.T) {
	m := newRecordingUpstream(t, func(t *testing.T, w http.ResponseWriter, r *http.Request, body []byte, n int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"cmpl-multi","model":"m-a","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Rasht\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":12,"total_tokens":42}}`)
	})
	ts := toolTestServer(t, m)

	payload := map[string]any{
		"model":       "main",
		"messages":    []any{map[string]any{"role": "user", "content": "What is the weather in Rasht?"}},
		"tools":       []any{weatherTool(), timeTool(), calcTool()},
		"tool_choice": forcedChoice(),
		"max_tokens":  64,
	}
	resp := postChatPayload(t, ts, payload)
	assertGuvnaStep(t, resp, "a/m-a")
	out := readChatResponse(t, resp)
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", out.Choices[0].FinishReason)
	}
	tcs := out.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(tcs))
	}
	args := assertValidToolCall(t, tcs[0])
	if tcs[0].Function.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", tcs[0].Function.Name)
	}
	if args["city"] != "Rasht" {
		t.Errorf("tool args = %v, want city=Rasht", args)
	}

	reqs := m.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
	upBody := bodyJSON(t, reqs[0])
	got := toolNames(t, upBody)
	want := []string{"get_weather", "get_time", "calculate"}
	if len(got) != len(want) {
		t.Fatalf("upstream tools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("upstream tool[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	tc, ok := upBody["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" {
		t.Fatalf("upstream tool_choice = %v, want forced function choice", upBody["tool_choice"])
	}
}

// runToolLoop runs a tool-calling agent loop through guvna: send the
// messages, execute any returned tool calls with exec, feed the results
// back, and repeat until the model answers with content or maxTurns is
// hit. It returns the final content and the number of turns used.
// firstChoice applies to turn 1, laterChoice to subsequent turns.
func runToolLoop(t *testing.T, ts *httptest.Server, tools []toolDef, firstChoice, laterChoice any, maxTurns int, exec func(toolCall) string) (string, int) {
	t.Helper()
	messages := []chatMessage{{Role: "user", Content: strptr("What is the weather in Rasht?")}}
	turns := 0
	for turn := 1; turn <= maxTurns; turn++ {
		turns = turn
		choice := laterChoice
		if turn == 1 {
			choice = firstChoice
		}
		payload := map[string]any{
			"model":       "main",
			"messages":    messages,
			"tools":       tools,
			"tool_choice": choice,
			"max_tokens":  128,
		}
		resp := postChatPayload(t, ts, payload)
		assertGuvnaStep(t, resp, "a/m-a")
		out := readChatResponse(t, resp)
		msg := out.Choices[0].Message
		t.Logf("turn %d: finish_reason=%q content=%v tool_calls=%d", turn, out.Choices[0].FinishReason, deref(msg.Content), len(msg.ToolCalls))
		if len(msg.ToolCalls) == 0 {
			if msg.Content == nil {
				t.Fatalf("turn %d: no tool calls and no content", turn)
			}
			return *msg.Content, turns
		}
		// Append the assistant message (with tool calls) and one tool
		// result per call, then loop.
		messages = append(messages, msg)
		for _, tc := range msg.ToolCalls {
			result := exec(tc)
			messages = append(messages, chatMessage{
				Role:       "tool",
				Content:    strptr(result),
				ToolCallID: tc.ID,
			})
		}
	}
	t.Fatalf("loop did not terminate within %d turns", maxTurns)
	return "", turns
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestChatMultiTurnToolLoop: a scripted upstream returns a tool call on turn 1
// and a final answer on turn 2 (once a tool-role message is present). The
// loop must terminate in 2 turns, and turn 2's request must carry the
// assistant tool_calls plus the executed tool result.
func TestChatMultiTurnToolLoop(t *testing.T) {
	m := newRecordingUpstream(t, func(t *testing.T, w http.ResponseWriter, r *http.Request, body []byte, n int) {
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		hasTool := false
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				hasTool = true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if hasTool {
			io.WriteString(w, `{"id":"cmpl-t2","model":"m-a","choices":[{"message":{"role":"assistant","content":"It is sunny in Rasht, 18C."},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":10,"total_tokens":50}}`)
			return
		}
		io.WriteString(w, `{"id":"cmpl-t1","model":"m-a","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_7","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Rasht\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":12,"total_tokens":32}}`)
	})
	ts := toolTestServer(t, m)

	exec := func(tc toolCall) string {
		if tc.Function.Name != "get_weather" {
			t.Fatalf("executor called for %q, want get_weather", tc.Function.Name)
		}
		return `{"city": "Rasht", "condition": "sunny", "temp_c": 18}`
	}
	final, turns := runToolLoop(t, ts, []toolDef{weatherTool()}, forcedChoice(), "auto", 3, exec)
	if turns != 2 {
		t.Errorf("turns = %d, want 2", turns)
	}
	if !strings.Contains(final, "sunny") {
		t.Errorf("final content = %q, want it to mention sunny", final)
	}

	reqs := m.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(reqs))
	}
	// Turn 2's request must contain the assistant tool_calls and the tool
	// result message.
	var turn2 struct {
		Messages []struct {
			Role       string     `json:"role"`
			Content    *string    `json:"content"`
			ToolCalls  []toolCall `json:"tool_calls"`
			ToolCallID string     `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqs[1], &turn2); err != nil {
		t.Fatal(err)
	}
	if len(turn2.Messages) != 3 {
		t.Fatalf("turn 2 messages = %d, want 3 (user, assistant, tool)", len(turn2.Messages))
	}
	if len(turn2.Messages[1].ToolCalls) != 1 || turn2.Messages[1].ToolCalls[0].ID != "call_7" {
		t.Errorf("turn 2 assistant message lost the tool call: %+v", turn2.Messages[1])
	}
	toolMsg := turn2.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_7" || toolMsg.Content == nil || !strings.Contains(*toolMsg.Content, "sunny") {
		t.Errorf("turn 2 tool message = %+v, want role=tool id=call_7 with the executed result", toolMsg)
	}
}
