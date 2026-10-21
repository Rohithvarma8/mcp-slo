package main

import (
	"os"
	"testing"

	"github.com/Rohithvarma8/mcp-slo/internal/mcptest"
)

func TestExitCodes(t *testing.T) {
	srv := mcptest.New(mcptest.Options{})
	defer srv.Close()
	t.Setenv("GITHUB_STEP_SUMMARY", t.TempDir()+"/summary.md")
	base := []string{"run", "--quiet", "--url", srv.URL, "--concurrency", "1", "--duration", "50ms", "--tool", "echo"}

	if code := run(append(base, "--slo", "p95<10s", "--slo", "errors<1%")); code != 0 {
		t.Errorf("passing run: exit %d", code)
	}
	if b, _ := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY")); len(b) == 0 {
		t.Error("step summary not written")
	}
	if code := run(append(base, "--slo", "p95<1ns")); code != 1 {
		t.Errorf("SLO breach: exit %d, want 1", code)
	}
	if code := run(append(base, "--slo", "p95>1s")); code != 2 {
		t.Errorf("bad SLO: exit %d, want 2", code)
	}
	if code := run([]string{"run", "--url", srv.URL, "--args", "[1]"}); code != 2 {
		t.Errorf("bad args: exit %d, want 2", code)
	}
}
