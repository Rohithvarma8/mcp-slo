package runner

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Rohithvarma8/mcp-slo/internal/mcptest"
)

func mustSLOs(t *testing.T, raw ...string) []SLO {
	t.Helper()
	var out []SLO
	for _, r := range raw {
		s, err := ParseSLO(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func quickRun(t *testing.T, srv *mcptest.Server, mod func(*Config)) *Report {
	t.Helper()
	cfg := Config{
		URL:         srv.URL,
		Concurrency: 2,
		Duration:    150 * time.Millisecond,
		Tool:        "echo",
		GetTimeout:  200 * time.Millisecond,
	}
	if mod != nil {
		mod(&cfg)
	}
	rep, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func findCheck(rep *Report, prefix string) *Check {
	for i := range rep.Checks {
		if strings.HasPrefix(rep.Checks[i].Name, prefix) {
			return &rep.Checks[i]
		}
	}
	return nil
}

func TestParseSLO(t *testing.T) {
	good := map[string]SLO{
		"p95<500ms":            {Metric: "p95", Latency: 500 * time.Millisecond},
		"errors<1%":            {Metric: "errors", Rate: 0.01},
		"tools/call:p99<=1.5s": {Method: "tools/call", Metric: "p99", Inclusive: true, Latency: 1500 * time.Millisecond},
		"max < 2s":             {Metric: "max", Latency: 2 * time.Second},
	}
	for in, want := range good {
		got, err := ParseSLO(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		want.Raw = got.Raw
		if got != want {
			t.Errorf("%q: got %+v want %+v", in, got, want)
		}
	}
	for _, in := range []string{"p95>500ms", "p42<1s", "errors<1", "p95<fast", "errors<200%"} {
		if _, err := ParseSLO(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestLoadJSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		srv := mcptest.New(mcptest.Options{SSE: sse})
		rep := quickRun(t, srv, func(c *Config) { c.SLOs = mustSLOs(t, "p95<1s", "errors<1%") })
		srv.Close()
		if !rep.Passed {
			t.Fatalf("sse=%v: expected pass, got %+v %+v", sse, rep.SLOs, rep.Checks)
		}
		got := map[string]int{}
		for _, m := range rep.Methods {
			got[m.Method] = m.Count
		}
		for _, m := range methodOrder {
			if got[m] == 0 {
				t.Errorf("sse=%v: no samples for %s", sse, m)
			}
		}
		if rep.Overall.Errors != 0 || rep.FirstError != "" {
			t.Errorf("sse=%v: unexpected errors: %s", sse, rep.FirstError)
		}
	}
}

func TestNegotiationMatrix(t *testing.T) {
	srv := mcptest.New(mcptest.Options{Versions: []string{"2025-03-26", "2025-06-18"}})
	defer srv.Close()
	rep := quickRun(t, srv, nil)
	want := []struct{ result, negotiated string }{
		{"accepted", "2025-03-26"},
		{"accepted", "2025-06-18"},
		{"downgraded", "2025-06-18"},
		{"downgraded", "2025-06-18"},
	}
	if len(rep.Negotiation) != len(want) {
		t.Fatalf("got %d rows", len(rep.Negotiation))
	}
	for i, w := range want {
		n := rep.Negotiation[i]
		if n.Result != w.result || n.Negotiated != w.negotiated || n.Status != 200 {
			t.Errorf("%s: got %+v, want %+v", n.Requested, n, w)
		}
	}
	if rep.ProtocolVersion != "2025-06-18" {
		t.Errorf("load should use newest accepted version, got %s", rep.ProtocolVersion)
	}
}

func TestSLOFailures(t *testing.T) {
	srv := mcptest.New(mcptest.Options{Latency: 20 * time.Millisecond})
	defer srv.Close()
	rep := quickRun(t, srv, func(c *Config) { c.SLOs = mustSLOs(t, "p95<5ms", "p95<1s") })
	if rep.Passed || rep.SLOs[0].Passed || !rep.SLOs[1].Passed {
		t.Fatalf("want p95<5ms to fail and p95<1s to pass: %+v", rep.SLOs)
	}

	bad := mcptest.New(mcptest.Options{ToolError: true})
	defer bad.Close()
	rep = quickRun(t, bad, func(c *Config) { c.SLOs = mustSLOs(t, "errors<1%", "tools/list:errors<1%") })
	if rep.SLOs[0].Passed || !rep.SLOs[1].Passed {
		t.Fatalf("want overall errors to fail, tools/list to pass: %+v", rep.SLOs)
	}
	if !strings.Contains(rep.FirstError, "tools/call: tool returned isError: boom") {
		t.Errorf("first error = %q", rep.FirstError)
	}

	rep = quickRun(t, srv, func(c *Config) { c.Tool = ""; c.SLOs = mustSLOs(t, "tools/call:p95<1s") })
	if rep.SLOs[0].Passed || rep.SLOs[0].Actual != "no samples" {
		t.Errorf("SLO on a method with no samples must fail: %+v", rep.SLOs[0])
	}
}

func TestUnreachableServerFails(t *testing.T) {
	srv := mcptest.New(mcptest.Options{})
	url := srv.URL
	srv.Close()
	rep, err := Run(context.Background(), Config{URL: url, Concurrency: 1, Duration: 50 * time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed || rep.FirstError == "" {
		t.Fatalf("unreachable server must fail with an error, got %+v", rep.Checks)
	}
}

func TestAuthProbes(t *testing.T) {
	open := mcptest.New(mcptest.Options{})
	defer open.Close()
	rep := quickRun(t, open, nil)
	if rep.Auth.Required || len(rep.Auth.Probes) != 0 || !strings.HasPrefix(rep.Auth.Summary, "no auth required") {
		t.Fatalf("open server: %+v", rep.Auth)
	}

	srv := mcptest.New(mcptest.Options{Token: "good", WWWAuth: true})
	defer srv.Close()
	rep = quickRun(t, srv, func(c *Config) { c.Header = http.Header{"Authorization": {"Bearer good"}} })
	if !rep.Auth.Required || len(rep.Auth.Probes) != 3 {
		t.Fatalf("auth server: %+v", rep.Auth)
	}
	for _, p := range rep.Auth.Probes {
		if p.Status != 401 || !p.WWWAuthenticate || !p.Passed {
			t.Errorf("probe %+v", p)
		}
	}
	if !rep.Passed {
		t.Errorf("authenticated load should pass: %s", rep.FirstError)
	}
}

func TestAlexaProfile(t *testing.T) {
	alexa := func(c *Config) { c.Profile = "alexa"; c.Header = http.Header{"Authorization": {"Bearer good"}} }

	withChallenge := mcptest.New(mcptest.Options{Token: "good", WWWAuth: true})
	defer withChallenge.Close()
	rep := quickRun(t, withChallenge, alexa)
	if rep.Passed {
		t.Fatal("alexa: 401 with WWW-Authenticate must fail")
	}
	for _, p := range rep.Auth.Probes {
		if p.Passed || !strings.Contains(p.Detail, "WWW-Authenticate") {
			t.Errorf("probe %+v", p)
		}
	}

	compliant := mcptest.New(mcptest.Options{Token: "good"})
	defer compliant.Close()
	rep = quickRun(t, compliant, alexa)
	if !rep.Passed {
		t.Fatalf("alexa: compliant server should pass: %+v", rep.Checks)
	}
	if c := findCheck(rep, "alexa: GET"); c == nil || !c.Passed || !strings.Contains(c.Detail, "405") {
		t.Errorf("GET check %+v", c)
	}

	hanging := mcptest.New(mcptest.Options{HangingGET: true})
	defer hanging.Close()
	rep = quickRun(t, hanging, func(c *Config) { c.Profile = "alexa" })
	if c := findCheck(rep, "alexa: GET"); c == nil || c.Passed || !strings.Contains(c.Detail, "stream open") {
		t.Fatalf("hanging GET must fail: %+v", c)
	}
	if rep.Passed {
		t.Error("report must fail on a hanging GET")
	}
}

func TestColdWait(t *testing.T) {
	srv := mcptest.New(mcptest.Options{})
	defer srv.Close()
	rep := quickRun(t, srv, func(c *Config) { c.ColdWait = 10 * time.Millisecond })
	if rep.Cold == nil || len(rep.Cold.Samples) != 4 || rep.Cold.Samples[0].Method != "initialize" {
		t.Fatalf("cold report %+v", rep.Cold)
	}
	if rep.Cold.Samples[0].SteadyP50 == 0 {
		t.Error("steady p50 not filled in")
	}
}

func TestRateLimit(t *testing.T) {
	srv := mcptest.New(mcptest.Options{})
	defer srv.Close()
	rep := quickRun(t, srv, func(c *Config) { c.Rate = 40; c.Duration = 500 * time.Millisecond; c.Concurrency = 3 })
	// 40 req/s for 0.5s is ~20 requests; allow slack for the in-flight tail.
	if n := rep.Overall.Count; n < 10 || n > 30 {
		t.Fatalf("rate 40/s over 500ms produced %d requests", n)
	}
}

func TestLatencyIgnoresFailures(t *testing.T) {
	r := NewRecorder()
	r.Record("tools/call", 100*time.Millisecond, nil)
	r.Record("tools/call", time.Millisecond, context.DeadlineExceeded)
	methods, overall, _ := r.Summary()
	if methods[0].Count != 2 || methods[0].Errors != 1 || methods[0].P50 != Duration(100*time.Millisecond) {
		t.Fatalf("stats %+v", methods[0])
	}

	failed := NewRecorder()
	failed.Record("initialize", time.Millisecond, context.DeadlineExceeded)
	methods, overall, _ = failed.Summary()
	res := Evaluate(mustSLOs(t, "p95<1s"), methods, overall)
	if res[0].Passed || res[0].Actual != "no successful requests" {
		t.Fatalf("latency SLO must fail when every request failed: %+v", res[0])
	}
}
