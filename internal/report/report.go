// Package report renders a runner.Report as a terminal table, JSON, JUnit XML
// and a GitHub step summary in Markdown.
package report

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Rohithvarma8/mcp-slo/internal/runner"
)

func dur(d runner.Duration) string { return runner.FormatDuration(time.Duration(d)) }

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func verdict(r *runner.Report) string {
	if r.Passed {
		return "PASS"
	}
	return "FAIL"
}

func failures(r *runner.Report) int {
	n := 0
	for _, c := range r.Checks {
		if !c.Passed {
			n++
		}
	}
	for _, s := range r.SLOs {
		if !s.Passed {
			n++
		}
	}
	return n
}

func negotiationDetail(n runner.Negotiation) string {
	if n.Error != "" {
		return n.Result + ": " + n.Error
	}
	return n.Result
}

func status(code int) string {
	if code == 0 {
		return "-"
	}
	return fmt.Sprint(code)
}

// Text writes the human-readable terminal report.
func Text(w io.Writer, r *runner.Report) {
	fmt.Fprintf(w, "\nmcp-slo %s  %s\n", r.Version, r.URL)
	server := ""
	if r.ServerName != "" {
		server = fmt.Sprintf(" · server %s %s", r.ServerName, r.ServerVersion)
	}
	profile := ""
	if r.Profile != "" {
		profile = " · profile " + r.Profile
	}
	rate := ""
	if r.Load.Rate > 0 {
		rate = fmt.Sprintf(" ≤ %g req/s", r.Load.Rate)
	}
	fmt.Fprintf(w, "%d workers × %s%s · protocol %s%s%s\n", r.Load.Concurrency, time.Duration(r.Load.Duration), rate, r.ProtocolVersion, server, profile)

	fmt.Fprintln(w, "\nPROTOCOL NEGOTIATION")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  requested\thttp\tnegotiated\tresult")
	for _, n := range r.Negotiation {
		neg := n.Negotiated
		if neg == "" {
			neg = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", n.Requested, status(n.Status), neg, negotiationDetail(n))
	}
	tw.Flush()

	fmt.Fprintln(w, "\nAUTH")
	if !r.Auth.Required {
		fmt.Fprintf(w, "  %s\n", r.Auth.Summary)
	} else {
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  \tprobe\thttp\tWWW-Authenticate\tdetail")
		for _, p := range r.Auth.Probes {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", mark(p.Passed), p.Name, status(p.Status), yesNo(p.WWWAuthenticate), p.Detail)
		}
		tw.Flush()
	}

	if r.Cold != nil {
		fmt.Fprintf(w, "\nCOLD START (first session after %s idle)\n", time.Duration(r.Cold.Wait))
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  method\tcold\tsteady p50\tratio")
		for _, s := range r.Cold.Samples {
			if s.Error != "" {
				fmt.Fprintf(tw, "  %s\terror\t%s\t%s\n", s.Method, dur(s.SteadyP50), s.Error)
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.Method, dur(s.Latency), dur(s.SteadyP50), ratio(s))
		}
		tw.Flush()
	}

	fmt.Fprintf(w, "\nLATENCY (steady state, successful requests; %d requests in %s, %.1f req/s)\n",
		r.Overall.Count, time.Duration(r.Load.Elapsed).Round(100*time.Millisecond), r.Load.RPS)
	rows := append(r.Methods, r.Overall)
	width := len("method")
	for _, m := range rows {
		width = max(width, len(m.Method))
	}
	// AlignRight for the numbers; the method column is pre-padded to stay left-aligned.
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  %-*s\tcount\terrors\tp50\tp95\tp99\tmax\t\n", width, "method")
	for _, m := range rows {
		fmt.Fprintf(tw, "  %-*s\t%d\t%s\t%s\t%s\t%s\t%s\t\n", width, m.Method, m.Count, runner.FormatPercent(m.ErrorRate),
			dur(m.P50), dur(m.P95), dur(m.P99), dur(m.Max))
	}
	tw.Flush()
	if r.FirstError != "" {
		fmt.Fprintf(w, "  first error: %s\n", r.FirstError)
	}

	fmt.Fprintln(w, "\nCHECKS")
	for _, c := range r.Checks {
		fmt.Fprintf(w, "  %s %s: %s\n", mark(c.Passed), c.Name, c.Detail)
	}

	if len(r.SLOs) > 0 {
		fmt.Fprintln(w, "\nSLOs")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, s := range r.SLOs {
			fmt.Fprintf(tw, "  %s %s\t%s\tactual %s\t%s\n", mark(s.Passed), s.SLO, s.Scope, s.Actual, s.Detail)
		}
		tw.Flush()
	}

	if r.Passed {
		fmt.Fprintln(w, "\nPASS")
	} else {
		fmt.Fprintf(w, "\nFAIL: %d check(s) failed\n", failures(r))
	}
}

func yesNo(b bool) string {
	if b {
		return "present"
	}
	return "absent"
}

func ratio(s runner.ColdSample) string {
	if s.SteadyP50 <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fx", float64(s.Latency)/float64(s.SteadyP50))
}

// JSON writes the machine-readable report.
func JSON(path string, r *runner.Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Skipped   int         `xml:"skipped,attr"`
	Timestamp string      `xml:"timestamp,attr"`
	Time      string      `xml:"time,attr"`
	Cases     []junitCase `xml:"testcase"`
}

type junitCase struct {
	Classname string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitFailure `xml:"skipped,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// JUnit writes one testcase per SLO, check and negotiated protocol version.
func JUnit(path string, r *runner.Report) error {
	suite := junitSuite{
		Name:      "mcp-slo " + r.URL,
		Timestamp: r.StartedAt.Format(time.RFC3339),
		Time:      fmt.Sprintf("%.3f", time.Duration(r.Load.Elapsed).Seconds()),
	}
	add := func(class, name string, ok bool, detail string) {
		c := junitCase{Classname: class, Name: name, SystemOut: detail}
		if !ok {
			c.Failure = &junitFailure{Message: detail, Text: detail}
			suite.Failures++
		}
		suite.Cases = append(suite.Cases, c)
	}
	for _, s := range r.SLOs {
		add("mcp-slo.slo", s.SLO+" ("+s.Scope+")", s.Passed, "actual "+s.Actual+" "+s.Detail)
	}
	for _, c := range r.Checks {
		add("mcp-slo.checks", c.Name, c.Passed, c.Detail)
	}
	for _, n := range r.Negotiation {
		c := junitCase{Classname: "mcp-slo.negotiation", Name: "protocol " + n.Requested, SystemOut: negotiationDetail(n)}
		if n.Result == "rejected" {
			c.Skipped = &junitFailure{Message: negotiationDetail(n)}
			suite.Skipped++
		}
		suite.Cases = append(suite.Cases, c)
	}
	suite.Tests = len(suite.Cases)
	doc := junitSuites{Name: "mcp-slo", Tests: suite.Tests, Failures: suite.Failures, Suites: []junitSuite{suite}}
	b, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(xml.Header), append(b, '\n')...), 0o644)
}

// Markdown writes a GitHub step summary.
func Markdown(w io.Writer, r *runner.Report) {
	icon := "✅"
	if !r.Passed {
		icon = "❌"
	}
	fmt.Fprintf(w, "## %s mcp-slo: %s\n\n", icon, verdict(r))
	fmt.Fprintf(w, "`%s` · %d workers × %s · protocol `%s`", r.URL, r.Load.Concurrency, time.Duration(r.Load.Duration), r.ProtocolVersion)
	if r.Profile != "" {
		fmt.Fprintf(w, " · profile `%s`", r.Profile)
	}
	fmt.Fprint(w, "\n\n")

	if len(r.SLOs) > 0 {
		fmt.Fprintln(w, "| | SLO | Scope | Actual |\n|---|---|---|---|")
		for _, s := range r.SLOs {
			fmt.Fprintf(w, "| %s | `%s` | %s | %s |\n", mark(s.Passed), s.SLO, s.Scope, s.Actual)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "**Latency** (%d requests, %.1f req/s)\n\n", r.Overall.Count, r.Load.RPS)
	fmt.Fprintln(w, "| Method | Count | Errors | p50 | p95 | p99 | Max |\n|---|--:|--:|--:|--:|--:|--:|")
	for _, m := range append(r.Methods, r.Overall) {
		fmt.Fprintf(w, "| `%s` | %d | %s | %s | %s | %s | %s |\n", m.Method, m.Count, runner.FormatPercent(m.ErrorRate),
			dur(m.P50), dur(m.P95), dur(m.P99), dur(m.Max))
	}
	if r.FirstError != "" {
		fmt.Fprintf(w, "\nFirst error: `%s`\n", strings.ReplaceAll(r.FirstError, "`", "'"))
	}

	if r.Cold != nil {
		fmt.Fprintf(w, "\n**Cold start** (after %s idle)\n\n| Method | Cold | Steady p50 |\n|---|--:|--:|\n", time.Duration(r.Cold.Wait))
		for _, s := range r.Cold.Samples {
			fmt.Fprintf(w, "| `%s` | %s | %s |\n", s.Method, dur(s.Latency), dur(s.SteadyP50))
		}
	}

	fmt.Fprintln(w, "\n**Checks**\n\n| | Check | Detail |\n|---|---|---|")
	for _, c := range r.Checks {
		fmt.Fprintf(w, "| %s | %s | %s |\n", mark(c.Passed), c.Name, c.Detail)
	}
	if !r.Auth.Required {
		fmt.Fprintf(w, "\nAuth: %s\n", r.Auth.Summary)
	}

	fmt.Fprintln(w, "\n**Protocol negotiation**\n\n| Requested | HTTP | Negotiated | Result |\n|---|--:|---|---|")
	for _, n := range r.Negotiation {
		fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n", n.Requested, status(n.Status), n.Negotiated, n.Result)
	}
	fmt.Fprintln(w)
}
