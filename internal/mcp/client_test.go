package mcp_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Rohithvarma8/mcp-slo/internal/mcp"
	"github.com/Rohithvarma8/mcp-slo/internal/mcptest"
)

func TestSessionJSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		name := "json"
		if sse {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			srv := mcptest.New(mcptest.Options{SSE: sse})
			defer srv.Close()
			c := &mcp.Client{URL: srv.URL}
			ctx := context.Background()

			s, res, _, err := c.Initialize(ctx, "2025-06-18")
			if err != nil {
				t.Fatal(err)
			}
			if res.ProtocolVersion != "2025-06-18" || res.ServerInfo.Name != "fake" {
				t.Fatalf("unexpected initialize result %+v", res)
			}
			if s.ID == "" {
				t.Fatal("session id not captured")
			}
			if resp, err := c.Notify(ctx, s, "notifications/initialized", nil); err != nil || resp.Status != 202 {
				t.Fatalf("notify: %v %+v", err, resp)
			}
			resp, err := c.Call(ctx, s, "tools/list", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(resp.Result), `"echo"`) {
				t.Fatalf("tools/list result %s", resp.Result)
			}
		})
	}
}

func TestRPCAndHTTPErrors(t *testing.T) {
	srv := mcptest.New(mcptest.Options{Token: "good"})
	defer srv.Close()
	c := &mcp.Client{URL: srv.URL}
	_, _, resp, err := c.Initialize(context.Background(), "2025-06-18")
	var he *mcp.HTTPError
	if !errors.As(err, &he) || he.Status != 401 || resp.Status != 401 {
		t.Fatalf("want HTTP 401, got %v", err)
	}

	open := mcptest.New(mcptest.Options{SSE: true})
	defer open.Close()
	c = &mcp.Client{URL: open.URL}
	_, err = c.Call(context.Background(), nil, "nope", nil)
	var re *mcp.RPCError
	if !errors.As(err, &re) || re.Code != -32601 {
		t.Fatalf("want JSON-RPC -32601, got %v", err)
	}
}
