// Command mcp-slo reliability-tests a Streamable HTTP MCP server against SLOs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Rohithvarma8/mcp-slo/internal/mcp"
	"github.com/Rohithvarma8/mcp-slo/internal/report"
	"github.com/Rohithvarma8/mcp-slo/internal/runner"
)

// version is set with -ldflags "-X main.version=..."; go install fills it from
// the module version otherwise.
var version = ""

const usage = `mcp-slo reliability-tests a Streamable HTTP MCP server against SLOs.

Usage:
  mcp-slo run --url <mcp-url> [flags]
  mcp-slo version

Exit codes: 0 all SLOs and checks passed, 1 a check or SLO failed, 2 usage error.

Run flags:
`

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	mcp.ClientVersion = resolveVersion()
	fs := newFlagSet()
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fs.fs.SetOutput(os.Stdout)
		fs.fs.Usage()
		return 0
	}
	switch args[0] {
	case "version", "--version":
		fmt.Println("mcp-slo", mcp.ClientVersion)
		return 0
	case "run":
	default:
		fmt.Fprintf(os.Stderr, "mcp-slo: unknown command %q\n\n", args[0])
		fs.fs.Usage()
		return 2
	}
	if err := fs.fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := fs.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp-slo:", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rep, err := runner.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp-slo:", err)
		return 2
	}

	report.Text(os.Stdout, rep)
	if *fs.jsonOut != "" {
		if err := report.JSON(*fs.jsonOut, rep); err != nil {
			fmt.Fprintln(os.Stderr, "mcp-slo: write JSON:", err)
			return 2
		}
	}
	if *fs.junitOut != "" {
		if err := report.JUnit(*fs.junitOut, rep); err != nil {
			fmt.Fprintln(os.Stderr, "mcp-slo: write JUnit:", err)
			return 2
		}
	}
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp-slo: write step summary:", err)
		} else {
			report.Markdown(f, rep)
			f.Close()
		}
	}
	if !rep.Passed {
		return 1
	}
	return 0
}

type flags struct {
	fs                                      *flag.FlagSet
	url, tool, args, profile, proto         *string
	jsonOut, junitOut                       *string
	concurrency                             *int
	rate                                    *float64
	duration, coldWait, timeout, getTimeout *time.Duration
	slos, headers                           multiFlag
	quiet                                   *bool
}

func newFlagSet() *flags {
	fs := flag.NewFlagSet("mcp-slo run", flag.ContinueOnError)
	f := &flags{fs: fs}
	f.url = fs.String("url", "", "Streamable HTTP MCP endpoint (required)")
	f.concurrency = fs.Int("concurrency", 5, "concurrent workers, each looping initialize → initialized → tools/list → tools/call")
	f.rate = fs.Float64("rate", 0, "max requests per second across all workers (0 = as fast as possible)")
	f.duration = fs.Duration("duration", 30*time.Second, "steady-state load duration")
	f.tool = fs.String("tool", "", "tool to call in each iteration (tools/call is skipped if empty)")
	f.args = fs.String("args", "{}", "JSON object of arguments for --tool")
	fs.Var(&f.slos, "slo", "SLO to enforce, repeatable: p50|p90|p95|p99|max|mean<dur, errors<N%, optionally prefixed by a method (tools/call:p95<300ms)")
	f.profile = fs.String("profile", "", "client profile with extra checks: alexa")
	f.coldWait = fs.Duration("cold-wait", 0, "idle for this long, then measure the first session separately from steady state")
	f.timeout = fs.Duration("timeout", 10*time.Second, "per-request timeout")
	f.getTimeout = fs.Duration("get-timeout", 3*time.Second, "alexa profile: a GET still open after this long counts as a hanging stream")
	f.proto = fs.String("protocol-version", "", "protocol version for the load test (default: newest the server accepts)")
	fs.Var(&f.headers, "header", "extra request header, repeatable: 'Authorization: Bearer $TOKEN'")
	f.jsonOut = fs.String("json", "", "write the full report as JSON to this file")
	f.junitOut = fs.String("junit", "", "write JUnit XML to this file")
	f.quiet = fs.Bool("quiet", false, "suppress progress messages on stderr")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	return f
}

func (f *flags) config() (runner.Config, error) {
	cfg := runner.Config{
		URL:             *f.url,
		Concurrency:     *f.concurrency,
		Rate:            *f.rate,
		Duration:        *f.duration,
		Tool:            *f.tool,
		Args:            json.RawMessage(*f.args),
		Profile:         *f.profile,
		ColdWait:        *f.coldWait,
		Timeout:         *f.timeout,
		GetTimeout:      *f.getTimeout,
		ProtocolVersion: *f.proto,
		Header:          http.Header{},
	}
	for _, s := range f.slos {
		slo, err := runner.ParseSLO(s)
		if err != nil {
			return cfg, err
		}
		cfg.SLOs = append(cfg.SLOs, slo)
	}
	for _, h := range f.headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return cfg, fmt.Errorf("invalid --header %q: want 'Name: value'", h)
		}
		cfg.Header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	if !*f.quiet {
		cfg.Logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "mcp-slo: "+format+"\n", args...)
		}
	}
	return cfg, nil
}

func resolveVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
