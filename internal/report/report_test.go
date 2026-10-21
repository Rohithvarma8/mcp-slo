package report

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rohithvarma8/mcp-slo/internal/mcptest"
	"github.com/Rohithvarma8/mcp-slo/internal/runner"
)

func TestOutputs(t *testing.T) {
	srv := mcptest.New(mcptest.Options{})
	defer srv.Close()
	slo, _ := runner.ParseSLO("p95<1ns")
	rep, err := runner.Run(context.Background(), runner.Config{
		URL: srv.URL, Concurrency: 1, Duration: 50 * time.Millisecond, Tool: "echo", SLOs: []runner.SLO{slo},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	var text bytes.Buffer
	Text(&text, rep)
	for _, want := range []string{"PROTOCOL NEGOTIATION", "no auth required", "tools/call", "✗ p95<1ns", "FAIL: 1 check(s) failed"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text output missing %q:\n%s", want, text.String())
		}
	}

	jsonPath := filepath.Join(dir, "out.json")
	if err := JSON(jsonPath, rep); err != nil {
		t.Fatal(err)
	}
	var decoded runner.Report
	b, _ := os.ReadFile(jsonPath)
	if err := json.Unmarshal(b, &decoded); err != nil || decoded.Overall.Count != rep.Overall.Count || decoded.Passed {
		t.Errorf("JSON round trip: %v %+v", err, decoded.Overall)
	}

	junitPath := filepath.Join(dir, "out.xml")
	if err := JUnit(junitPath, rep); err != nil {
		t.Fatal(err)
	}
	var suites junitSuites
	b, _ = os.ReadFile(junitPath)
	if err := xml.Unmarshal(b, &suites); err != nil {
		t.Fatal(err)
	}
	if suites.Failures != 1 || suites.Tests != len(rep.SLOs)+len(rep.Checks)+len(rep.Negotiation) {
		t.Errorf("junit tests=%d failures=%d", suites.Tests, suites.Failures)
	}

	var md bytes.Buffer
	Markdown(&md, rep)
	if !strings.HasPrefix(md.String(), "## ❌ mcp-slo: FAIL") || !strings.Contains(md.String(), "| `tools/call` |") {
		t.Errorf("markdown:\n%s", md.String())
	}
}
