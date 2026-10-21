// Package mcptest provides a configurable in-process MCP server for tests.
package mcptest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"
)

type Options struct {
	SSE         bool          // answer with text/event-stream instead of JSON
	Versions    []string      // supported protocol versions, oldest first
	Token       string        // if set, require "Authorization: Bearer <Token>"
	WWWAuth     bool          // add WWW-Authenticate to 401s
	Latency     time.Duration // added to every response
	ToolError   bool          // tools/call returns isError
	HangingGET  bool          // GET opens an SSE stream and never closes it
	StatelessID bool          // omit Mcp-Session-Id
}

type Server struct {
	*httptest.Server
	Opts     Options
	Requests atomic.Int64
}

func New(opts Options) *Server {
	if len(opts.Versions) == 0 {
		opts.Versions = []string{"2025-03-26", "2025-06-18", "2025-11-25"}
	}
	s := &Server{Opts: opts}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.Requests.Add(1)
	if s.Opts.Token != "" && r.Header.Get("Authorization") != "Bearer "+s.Opts.Token {
		if s.Opts.WWWAuth {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`)
		}
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !s.Opts.HangingGET {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var m msg
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	time.Sleep(s.Opts.Latency)
	if m.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var result any
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		negotiated := s.Opts.Versions[len(s.Opts.Versions)-1]
		for _, v := range s.Opts.Versions {
			if v == p.ProtocolVersion {
				negotiated = v
			}
		}
		if !s.Opts.StatelessID {
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("sess-%d", s.Requests.Load()))
		}
		result = map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake", "version": "1.0.0"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
	case "tools/call":
		result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "boom"}},
			"isError": s.Opts.ToolError,
		}
	default:
		s.write(w, map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	s.write(w, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
}

func (s *Server) write(w http.ResponseWriter, resp any) {
	b, _ := json.Marshal(resp)
	if !s.Opts.SSE {
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	// A progress notification first, so clients must match the response by id.
	fmt.Fprint(w, ": keepalive\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
	fmt.Fprintf(w, "event: message\nid: 1\ndata: %s\n\n", b)
}
