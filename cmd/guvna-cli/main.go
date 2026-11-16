// guvna-cli — local and remote status, logs, and chain management for a
// Guvna gateway. Local: talks to http://127.0.0.1:20128 (or GUVNA_URL).
// Remote: point --url at the gateway's HTTPS endpoint (caddy) — the admin
// key travels as a Bearer token; no new port is exposed.
//
//	guvna-cli status [--url URL] [--token KEY]
//	guvna-cli logs [-n 100] [--url URL] [--token KEY]
//	guvna-cli chains [list]
//	guvna-cli chains add NAME --step provider:model [--step ...]
//	guvna-cli chains rm NAME
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultURL = "http://127.0.0.1:20128"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "status":
		err = statusCmd(os.Args[2:])
	case "logs":
		err = logsCmd(os.Args[2:])
	case "chains":
		err = chainsCmd(os.Args[2:])
	case "providers":
		err = providersCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "guvna-cli: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `guvna-cli — gateway status, logs, chain management

usage:
  guvna-cli status [flags]
  guvna-cli logs [-n lines] [flags]
  guvna-cli chains [list]
  guvna-cli chains add NAME --step provider:model [--step ...]
  guvna-cli chains rm NAME
  guvna-cli providers [list]
  guvna-cli providers test [--provider NAME] [--model NAME]

flags:
  --url    gateway base URL (default: %s or $GUVNA_URL)
  --token  admin key (default: $GUVNA_ADMIN_KEY)
  --json   emit raw JSON instead of a table
`, defaultURL)
}

type commonFlags struct {
	url   string
	token string
	json  bool
}

func parseCommon(fs *flag.FlagSet, cf *commonFlags) {
	fs.StringVar(&cf.url, "url", envOr("GUVNA_URL", defaultURL), "gateway base URL")
	fs.StringVar(&cf.token, "token", os.Getenv("GUVNA_ADMIN_KEY"), "admin key (Bearer token)")
	fs.BoolVar(&cf.json, "json", false, "raw JSON output")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (cf commonFlags) get(path string, out any) error {
	return cf.do(http.MethodGet, path, nil, out)
}

// do performs an HTTP request against the gateway with the Bearer token and
// decodes a JSON response body into out (when out is non-nil).
func (cf commonFlags) do(method, path string, body any, out any) error {
	if cf.token == "" {
		return fmt.Errorf("no admin key: pass --token or set GUVNA_ADMIN_KEY")
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(cf.url, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cf.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(eb)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type statusResp struct {
	Uptime    string      `json:"uptime"`
	Chains    []string    `json:"chains"`
	Steps     []step      `json:"steps"`
	StepsDown int         `json:"steps_down"`
	Keys      []keyStatus `json:"keys"`
	Usage     struct {
		Requests   int            `json:"requests"`
		Streams    int            `json:"streams"`
		Errors     int            `json:"errors"`
		TokensIn   int64          `json:"tokens_in"`
		TokensOut  int64          `json:"tokens_out"`
		ByProvider []providerStat `json:"by_provider"`
	} `json:"usage"`
}

type keyStatus struct {
	Provider  string `json:"provider"`
	Index     int    `json:"index"`
	Env       string `json:"env"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	Failures  int    `json:"failures"`
	DownFor   int64  `json:"down_for_ns"`
	Requests  int64  `json:"requests"`
	TokensIn  int64  `json:"tokens_in"`
	TokensOut int64  `json:"tokens_out"`
}

type step struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Failures int    `json:"failures"`
	Down     bool   `json:"down"`
	DownFor  int64  `json:"down_for_ns"`
}

type providerStat struct {
	Provider  string `json:"provider"`
	Requests  int    `json:"requests"`
	Errors    int    `json:"errors"`
	TokensIn  int64  `json:"tokens_in"`
	TokensOut int64  `json:"tokens_out"`
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var cf commonFlags
	parseCommon(fs, &cf)
	fs.Parse(args)

	var st statusResp
	if err := cf.get("/admin/status", &st); err != nil {
		return err
	}
	if cf.json {
		return printJSON(st)
	}

	fmt.Printf("Guvna %s  (uptime %s)\n", cf.url, st.Uptime)
	fmt.Printf("chains: %s\n\n", strings.Join(st.Chains, "  "))

	fmt.Printf("%-12s %-28s %8s  %s\n", "PROVIDER", "MODEL", "FAILURES", "STATE")
	for _, s := range st.Steps {
		state := "ok"
		if s.Down {
			state = fmt.Sprintf("DOWN %v", time.Duration(s.DownFor))
		}
		fmt.Printf("%-12s %-28s %8d  %s\n", s.Provider, s.Model, s.Failures, state)
	}
	if st.StepsDown > 0 {
		fmt.Printf("\n%d step(s) down — skipped in chains until cool-off expires\n", st.StepsDown)
	}

	if len(st.Keys) > 0 {
		fmt.Printf("\nKEYS\n")
		fmt.Printf("  %-12s %-22s %-18s %-11s %8s  %10s\n", "PROVIDER", "KEY", "STATE", "REASON", "FAILURES", "TOKENS IN/OUT")
		for _, k := range st.Keys {
			state := k.State
			if k.State == "quarantined" && k.DownFor > 0 {
				state = fmt.Sprintf("quarantined %v", time.Duration(k.DownFor).Round(time.Second))
			}
			reason := k.Reason
			if reason == "" {
				reason = "-"
			}
			fmt.Printf("  %-12s %-22s %-18s %-11s %8d  %6d/%-6d\n",
				k.Provider, k.Env, state, reason, k.Failures, k.TokensIn, k.TokensOut)
		}
	}

	fmt.Printf("\nUSAGE (persisted)\n")
	fmt.Printf("  requests %d   streams %d   errors %d\n", st.Usage.Requests, st.Usage.Streams, st.Usage.Errors)
	fmt.Printf("  tokens %d in / %d out\n", st.Usage.TokensIn, st.Usage.TokensOut)
	if len(st.Usage.ByProvider) > 0 {
		fmt.Printf("\n  %-12s %8s %8s %12s %12s\n", "PROVIDER", "REQUESTS", "ERRORS", "TOKENS IN", "TOKENS OUT")
		for _, p := range st.Usage.ByProvider {
			fmt.Printf("  %-12s %8d %8d %12d %12d\n", p.Provider, p.Requests, p.Errors, p.TokensIn, p.TokensOut)
		}
	}
	return nil
}

func logsCmd(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	var cf commonFlags
	parseCommon(fs, &cf)
	n := fs.Int("n", 100, "number of lines to fetch (max 512)")
	fs.Parse(args)

	var out struct {
		Lines []string `json:"lines"`
	}
	if err := cf.get(fmt.Sprintf("/admin/logs?n=%d", *n), &out); err != nil {
		return err
	}
	if cf.json {
		return printJSON(out)
	}
	for _, l := range out.Lines {
		fmt.Println(l)
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type chainResp struct {
	Chains []chainInfo `json:"chains"`
}

type chainInfo struct {
	Name   string      `json:"name"`
	Source string      `json:"source"`
	Steps  []chainStep `json:"steps"`
}

type chainStep struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func chainsCmd(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return chainsListCmd(args)
	}
	switch args[0] {
	case "add":
		return chainsAddCmd(args[1:])
	case "rm", "remove":
		return chainsRmCmd(args[1:])
	default:
		return fmt.Errorf("unknown chains subcommand %q (want list|add|rm)", args[0])
	}
}

func chainsListCmd(args []string) error {
	fs := flag.NewFlagSet("chains", flag.ExitOnError)
	var cf commonFlags
	parseCommon(fs, &cf)
	fs.Parse(args)

	var out chainResp
	if err := cf.get("/v1/chains", &out); err != nil {
		return err
	}
	if cf.json {
		return printJSON(out)
	}
	fmt.Printf("%-20s %-8s  %s\n", "NAME", "SOURCE", "STEPS")
	for _, c := range out.Chains {
		steps := make([]string, 0, len(c.Steps))
		for _, s := range c.Steps {
			steps = append(steps, s.Provider+":"+s.Model)
		}
		fmt.Printf("%-20s %-8s  %s\n", c.Name, c.Source, strings.Join(steps, " -> "))
	}
	return nil
}

// reorderArgs moves flag tokens (and their values) before positional args so
// Go's flag package, which stops at the first non-flag argument, still parses
// commands like "chains add mychain --step a:b".
func reorderArgs(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && a != "-" && a != "--" && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func chainsAddCmd(args []string) error {
	var steps []chainStep
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--step" || args[i] == "-step" {
			if i+1 >= len(args) {
				return fmt.Errorf("--step needs provider:model")
			}
			p, m, found := strings.Cut(args[i+1], ":")
			if !found || p == "" || m == "" {
				return fmt.Errorf("bad step %q — want provider:model", args[i+1])
			}
			steps = append(steps, chainStep{Provider: p, Model: m})
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	if len(steps) == 0 {
		return fmt.Errorf("chains add needs at least one --step")
	}
	fs := flag.NewFlagSet("chains add", flag.ExitOnError)
	var cf commonFlags
	parseCommon(fs, &cf)
	fs.Parse(reorderArgs(rest))

	name := fs.Arg(0)
	if name == "" {
		return fmt.Errorf("chains add NAME --step provider:model [--step ...]")
	}
	var created chainInfo
	if err := cf.do(http.MethodPost, "/v1/chains", map[string]any{"name": name, "steps": steps}, &created); err != nil {
		return err
	}
	if cf.json {
		return printJSON(created)
	}
	fmt.Printf("created chain %q (%d steps, persisted)\n", created.Name, len(created.Steps))
	return nil
}

func chainsRmCmd(args []string) error {
	fs := flag.NewFlagSet("chains rm", flag.ExitOnError)
	var cf commonFlags
	parseCommon(fs, &cf)
	fs.Parse(reorderArgs(args))

	name := fs.Arg(0)
	if name == "" {
		return fmt.Errorf("chains rm NAME")
	}
	if err := cf.do(http.MethodDelete, "/v1/chains/"+name, nil, nil); err != nil {
		return err
	}
	fmt.Printf("removed chain %q\n", name)
	return nil
}

func providersCmd(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return providersListCmd(args)
	}
	switch args[0] {
	case "test":
		return providersTestCmd(args[1:])
	default:
		return fmt.Errorf("unknown providers subcommand %q (want list|test)", args[0])
	}
}

// providersListCmd renders per-provider pool summaries from /admin/status:
// provider, key count, and how many keys are healthy vs quarantined.
func providersListCmd(args []string) error {
	fs := flag.NewFlagSet("providers list", flag.ExitOnError)
	var cf commonFlags
	parseCommon(fs, &cf)
	fs.Parse(args)

	var st statusResp
	if err := cf.get("/admin/status", &st); err != nil {
		return err
	}
	if cf.json {
		return printJSON(st.Keys)
	}
	fmt.Printf("%-14s %6s %9s %13s\n", "PROVIDER", "KEYS", "HEALTHY", "QUARANTINED")
	type row struct{ total, healthy, quarantined int }
	order := []string{}
	byProvider := map[string]*row{}
	for _, k := range st.Keys {
		r, ok := byProvider[k.Provider]
		if !ok {
			r = &row{}
			byProvider[k.Provider] = r
			order = append(order, k.Provider)
		}
		r.total++
		if k.State == "quarantined" {
			r.quarantined++
		} else {
			r.healthy++
		}
	}
	for _, name := range order {
		r := byProvider[name]
		fmt.Printf("%-14s %6d %9d %13d\n", name, r.total, r.healthy, r.quarantined)
	}
	if len(order) == 0 {
		fmt.Println("(no providers — check config.yaml)")
	}
	return nil
}

type providerProbe struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	Note  string `json:"note"`
	Keys  []struct {
		Env       string `json:"env"`
		Verdict   string `json:"verdict"`
		Status    int    `json:"status"`
		LatencyMS int64  `json:"latency_ms"`
		Detail    string `json:"detail"`
	} `json:"keys"`
}

// providersTestCmd probes every configured provider's keys (or one provider /
// one model) through POST /admin/providers/test and renders verdicts.
func providersTestCmd(args []string) error {
	fs := flag.NewFlagSet("providers test", flag.ExitOnError)
	var cf commonFlags
	parseCommon(fs, &cf)
	provider := fs.String("provider", "", "probe only this provider")
	model := fs.String("model", "", "probe with this model instead of the provider's first configured one")
	fs.Parse(args)

	body := map[string]string{}
	if *provider != "" {
		body["provider"] = *provider
	}
	if *model != "" {
		body["model"] = *model
	}
	var out struct {
		Providers []providerProbe `json:"providers"`
	}
	if err := cf.do(http.MethodPost, "/admin/providers/test", body, &out); err != nil {
		return err
	}
	if cf.json {
		return printJSON(out)
	}
	fmt.Printf("%-14s %-28s %-20s %-13s %6s  %8s  %s\n",
		"PROVIDER", "MODEL", "KEY", "VERDICT", "STATUS", "LATENCY", "DETAIL")
	for _, p := range out.Providers {
		if p.Note != "" {
			fmt.Printf("%-14s %-28s %-20s %-13s %6s  %8s  %s\n", p.Name, "-", "-", "skipped", "-", "-", p.Note)
			continue
		}
		for _, k := range p.Keys {
			status := "-"
			if k.Status != 0 {
				status = fmt.Sprintf("%d", k.Status)
			}
			fmt.Printf("%-14s %-28s %-20s %-13s %6s  %8s  %s\n",
				p.Name, p.Model, k.Env, k.Verdict, status,
				fmt.Sprintf("%dms", k.LatencyMS), k.Detail)
		}
	}
	fmt.Printf("\nverdicts: ok / dead (401/403) / limited (429) / upstream (5xx) / unreachable (network) / rejected (other 4xx) / unset (env not set)\n")
	fmt.Printf("probes are read-only diagnostics: they do not touch key quarantine, step health, or telemetry.\n")
	return nil
}
