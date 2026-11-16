package metrics

import (
	"strings"
	"sync"
	"testing"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestCounterExposition(t *testing.T) {
	r := NewRegistry()
	r.IncCounter(ChatRequests, "Chat requests.", 1, Labels{"chain": "kilofree", "outcome": OutcomeSuccess})
	r.IncCounter(ChatRequests, "Chat requests.", 2, Labels{"chain": "kilofree", "outcome": OutcomeSuccess})
	r.IncCounter(ChatRequests, "Chat requests.", 1, Labels{"chain": "kilofree", "outcome": OutcomeFailed})

	out := render(t, r)
	want := "# HELP guvna_chat_requests_total Chat requests.\n" +
		"# TYPE guvna_chat_requests_total counter\n" +
		"guvna_chat_requests_total{chain=\"kilofree\",outcome=\"failed\"} 1\n" +
		"guvna_chat_requests_total{chain=\"kilofree\",outcome=\"success\"} 3\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := NewRegistry()
	r.ObserveHistogram(StepDuration, "Step latency.", []float64{1, 5, 10}, 0.5, Labels{"provider": "kilo"})
	r.ObserveHistogram(StepDuration, "Step latency.", []float64{1, 5, 10}, 7, Labels{"provider": "kilo"})

	out := render(t, r)
	for _, line := range []string{
		`guvna_step_duration_seconds_bucket{le="1",provider="kilo"} 1`,
		`guvna_step_duration_seconds_bucket{le="5",provider="kilo"} 1`,
		`guvna_step_duration_seconds_bucket{le="10",provider="kilo"} 2`,
		`guvna_step_duration_seconds_bucket{le="+Inf",provider="kilo"} 2`,
		`guvna_step_duration_seconds_sum{provider="kilo"} 7.5`,
		`guvna_step_duration_seconds_count{provider="kilo"} 2`,
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("missing line %q in:\n%s", line, out)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	r.IncCounter(ChatRequests, "x", 1, Labels{"chain": "a\"b\\c\nd"})
	out := render(t, r)
	if !strings.Contains(out, `chain="a\"b\\c\nd"`) {
		t.Fatalf("label not escaped in:\n%s", out)
	}
}

func TestGaugeFuncSampledAtScrape(t *testing.T) {
	r := NewRegistry()
	healthy := 2
	r.RegisterGaugeFunc(KeypoolKeys, "Keys.", func() []Sample {
		return []Sample{
			NewSample(float64(healthy), Labels{"provider": "kilo", "state": "healthy"}),
			NewSample(1, Labels{"provider": "kilo", "state": "quarantined"}),
		}
	})
	healthy = 0
	out := render(t, r)
	if !strings.Contains(out, `guvna_keypool_keys{provider="kilo",state="healthy"} 0`) {
		t.Fatalf("gauge func not re-evaluated at scrape:\n%s", out)
	}
}

func TestSetGauge(t *testing.T) {
	r := NewRegistry()
	r.SetGauge(Uptime, "Uptime.", 42.5, nil)
	out := render(t, r)
	if !strings.Contains(out, "guvna_uptime_seconds 42.5") {
		t.Fatalf("missing gauge in:\n%s", out)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.IncCounter(StepAttempts, "x", 1, Labels{"provider": "kilo", "outcome": OutcomeSuccess})
				r.ObserveHistogram(StepDuration, "x", DefaultBuckets, 0.3, Labels{"provider": "kilo"})
				var b strings.Builder
				_ = r.Write(&b)
			}
		}()
	}
	wg.Wait()
	out := render(t, r)
	if !strings.Contains(out, `guvna_step_attempts_total{outcome="success",provider="kilo"} 1600`) {
		t.Fatalf("lost increments in:\n%s", out)
	}
}
