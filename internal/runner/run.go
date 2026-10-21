// Package runner drives the reliability test: protocol negotiation, auth
// probes, an optional cold-start measurement and the concurrent load loop.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Rohithvarma8/mcp-slo/internal/mcp"
)

type Config struct {
	URL             string
	Concurrency     int
	Rate            float64 // max requests per second across workers; 0 = unlimited
	Duration        time.Duration
	Tool            string
	Args            json.RawMessage
	SLOs            []SLO
	Profile         string // "" or "alexa"
	ColdWait        time.Duration
	Timeout         time.Duration // per request
	GetTimeout      time.Duration // alexa GET hang threshold
	ProtocolVersion string        // overrides the negotiated choice
	Versions        []string      // negotiation matrix
	Header          http.Header
	Logf            func(format string, args ...any)
}

type Report struct {
	Tool            string        `json:"tool"`
	Version         string        `json:"version"`
	URL             string        `json:"url"`
	StartedAt       time.Time     `json:"started_at"`
	Profile         string        `json:"profile,omitempty"`
	ProtocolVersion string        `json:"protocol_version"`
	ServerName      string        `json:"server_name,omitempty"`
	ServerVersion   string        `json:"server_version,omitempty"`
	Negotiation     []Negotiation `json:"negotiation"`
	Auth            AuthReport    `json:"auth"`
	Checks          []Check       `json:"checks"`
	Cold            *ColdReport   `json:"cold,omitempty"`
	Load            LoadInfo      `json:"load"`
	Methods         []MethodStats `json:"methods"`
	Overall         MethodStats   `json:"overall"`
	FirstError      string        `json:"first_error,omitempty"`
	SLOs            []SLOResult   `json:"slos"`
	Passed          bool          `json:"passed"`
}

type LoadInfo struct {
	Concurrency int      `json:"concurrency"`
	Rate        float64  `json:"rate_limit,omitempty"`
	Duration    Duration `json:"duration_ms"`
	Elapsed     Duration `json:"elapsed_ms"`
	Tool        string   `json:"tool,omitempty"`
	RPS         float64  `json:"requests_per_second"`
}

// ColdReport compares the first iteration after an idle wait with steady state.
type ColdReport struct {
	Wait    Duration     `json:"wait_ms"`
	Samples []ColdSample `json:"samples"`
}

type ColdSample struct {
	Method    string   `json:"method"`
	Latency   Duration `json:"latency_ms"`
	SteadyP50 Duration `json:"steady_p50_ms"`
	Error     string   `json:"error,omitempty"`
}

func (cfg *Config) defaults() error {
	if cfg.URL == "" {
		return errors.New("--url is required")
	}
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return fmt.Errorf("--url must be http(s), got %q", cfg.URL)
	}
	if cfg.Rate < 0 {
		return errors.New("--rate must not be negative")
	}
	if cfg.Concurrency < 1 {
		return errors.New("--concurrency must be at least 1")
	}
	if cfg.Duration <= 0 {
		return errors.New("--duration must be positive")
	}
	if cfg.Profile != "" && cfg.Profile != "alexa" {
		return fmt.Errorf("unknown --profile %q (supported: alexa)", cfg.Profile)
	}
	if len(cfg.Args) == 0 {
		cfg.Args = json.RawMessage(`{}`)
	}
	var obj map[string]any
	if err := json.Unmarshal(cfg.Args, &obj); err != nil {
		return fmt.Errorf("--args must be a JSON object: %w", err)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.GetTimeout <= 0 {
		cfg.GetTimeout = 3 * time.Second
	}
	if len(cfg.Versions) == 0 {
		cfg.Versions = mcp.KnownProtocolVersions
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return nil
}

// Run executes the full test. The error is non-nil only for invalid config;
// server failures are reported in the Report.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = cfg.Concurrency * 2
	client := &mcp.Client{
		URL:    cfg.URL,
		HTTP:   &http.Client{Timeout: cfg.Timeout, Transport: transport},
		Header: cfg.Header,
	}
	rep := &Report{
		Tool:      "mcp-slo",
		Version:   mcp.ClientVersion,
		URL:       cfg.URL,
		StartedAt: time.Now().UTC(),
		Profile:   cfg.Profile,
		Load:      LoadInfo{Concurrency: cfg.Concurrency, Rate: cfg.Rate, Duration: Duration(cfg.Duration), Tool: cfg.Tool},
	}

	cfg.Logf("negotiating protocol versions %s", strings.Join(cfg.Versions, ", "))
	rep.Negotiation = negotiate(ctx, client, cfg.Versions)
	rep.ProtocolVersion = cfg.ProtocolVersion
	if rep.ProtocolVersion == "" {
		rep.ProtocolVersion = loadVersion(rep.Negotiation)
	}
	if s, res, _, err := client.Initialize(ctx, rep.ProtocolVersion); err == nil {
		rep.ServerName, rep.ServerVersion = res.ServerInfo.Name, res.ServerInfo.Version
		client.Close(s)
	}

	cfg.Logf("probing auth")
	rep.Auth = probeAuth(ctx, client, cfg.Profile)
	for _, p := range rep.Auth.Probes {
		rep.Checks = append(rep.Checks, Check{Name: "auth: " + p.Name, Passed: p.Passed, Detail: p.Detail})
	}
	if cfg.Profile == "alexa" {
		rep.Checks = append(rep.Checks, checkGET(ctx, client, cfg.GetTimeout))
	}

	if cfg.ColdWait > 0 {
		cfg.Logf("waiting %s before the cold-start request", cfg.ColdWait)
		select {
		case <-time.After(cfg.ColdWait):
		case <-ctx.Done():
		}
		rec := NewRecorder()
		iterate(ctx, client, rep.ProtocolVersion, cfg, rec, nil)
		methods, _, _ := rec.Summary()
		rep.Cold = &ColdReport{Wait: Duration(cfg.ColdWait)}
		for _, m := range methods {
			rep.Cold.Samples = append(rep.Cold.Samples, ColdSample{Method: m.Method, Latency: m.Max, Error: m.FirstError})
		}
	}

	cfg.Logf("load: %d workers for %s at protocol %s", cfg.Concurrency, cfg.Duration, rep.ProtocolVersion)
	rec := NewRecorder()
	pace := newPacer(cfg.Rate)
	start := time.Now()
	deadline := start.Add(cfg.Duration)
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) && ctx.Err() == nil {
				iterate(ctx, client, rep.ProtocolVersion, cfg, rec, pace)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	rep.Load.Elapsed = Duration(elapsed)
	rep.Methods, rep.Overall, rep.FirstError = rec.Summary()
	rep.Load.RPS = float64(rep.Overall.Count) / elapsed.Seconds()

	if rep.Cold != nil {
		for i, s := range rep.Cold.Samples {
			for _, m := range rep.Methods {
				if m.Method == s.Method {
					rep.Cold.Samples[i].SteadyP50 = m.P50
				}
			}
		}
	}

	answered := rep.Overall.Count > rep.Overall.Errors
	rep.Checks = append(rep.Checks, Check{
		Name:   "load: server answered requests",
		Passed: answered,
		Detail: fmt.Sprintf("%d of %d requests succeeded", rep.Overall.Count-rep.Overall.Errors, rep.Overall.Count),
	})
	rep.SLOs = Evaluate(cfg.SLOs, rep.Methods, rep.Overall)

	rep.Passed = true
	for _, c := range rep.Checks {
		rep.Passed = rep.Passed && c.Passed
	}
	for _, s := range rep.SLOs {
		rep.Passed = rep.Passed && s.Passed
	}
	return rep, nil
}

// iterate runs one session: initialize, initialized, tools/list and the
// optional tools/call.
func iterate(ctx context.Context, c *mcp.Client, version string, cfg Config, rec *Recorder, pace *pacer) {
	pace.wait(ctx)
	t := time.Now()
	s, _, _, err := c.Initialize(ctx, version)
	rec.Record("initialize", time.Since(t), err)
	if err != nil {
		return
	}
	defer c.Close(s)

	pace.wait(ctx)
	t = time.Now()
	_, err = c.Notify(ctx, s, "notifications/initialized", nil)
	rec.Record("notifications/initialized", time.Since(t), err)

	pace.wait(ctx)
	t = time.Now()
	_, err = c.Call(ctx, s, "tools/list", map[string]any{})
	rec.Record("tools/list", time.Since(t), err)

	if cfg.Tool == "" {
		return
	}
	pace.wait(ctx)
	t = time.Now()
	resp, err := c.Call(ctx, s, "tools/call", map[string]any{"name": cfg.Tool, "arguments": cfg.Args})
	if err == nil {
		err = toolError(resp.Result)
	}
	rec.Record("tools/call", time.Since(t), err)
}

// toolError turns a CallToolResult with isError=true into an error.
func toolError(result json.RawMessage) error {
	var r struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return fmt.Errorf("decode tools/call result: %w", err)
	}
	if !r.IsError {
		return nil
	}
	for _, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			msg := c.Text
			if len(msg) > 200 {
				msg = msg[:200] + "…"
			}
			return errors.New("tool returned isError: " + msg)
		}
	}
	return errors.New("tool returned isError")
}

// pacer spaces request starts evenly to enforce --rate across all workers.
type pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newPacer(rate float64) *pacer {
	if rate <= 0 {
		return nil
	}
	return &pacer{interval: time.Duration(float64(time.Second) / rate)}
}

func (p *pacer) wait(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	at := p.next
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()
	select {
	case <-time.After(time.Until(at)):
	case <-ctx.Done():
	}
}
