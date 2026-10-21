// Command fakeserver runs the test MCP server on a random local port and
// prints its URL. CI uses it to exercise the GitHub Action end to end.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/Rohithvarma8/mcp-slo/internal/mcptest"
)

func main() {
	sse := flag.Bool("sse", false, "answer with text/event-stream")
	latency := flag.Duration("latency", 5*time.Millisecond, "added latency per response")
	flag.Parse()
	srv := mcptest.New(mcptest.Options{SSE: *sse, Latency: *latency})
	defer srv.Close()
	fmt.Println(srv.URL)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}
