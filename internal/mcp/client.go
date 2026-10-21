// Package mcp is a minimal MCP client for the Streamable HTTP transport.
// It speaks just enough JSON-RPC to initialize a session, list tools and call
// them, and it accepts both application/json and text/event-stream responses.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// KnownProtocolVersions are the MCP revisions probed by the negotiation matrix,
// oldest first.
var KnownProtocolVersions = []string{"2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28"}

// ClientVersion is reported to servers in clientInfo.
var ClientVersion = "dev"

var nextID atomic.Int64

// Client sends JSON-RPC messages to a single MCP endpoint. It is safe for
// concurrent use and cheap to copy (e.g. to swap Header for an auth probe).
type Client struct {
	URL    string
	HTTP   *http.Client
	Header http.Header // extra headers sent on every request
}

// Session carries the state the server handed back from initialize.
type Session struct {
	ID              string
	ProtocolVersion string
}

// Response is the HTTP-level outcome of a request.
type Response struct {
	Status int
	Header http.Header
	Result json.RawMessage
}

// HTTPError is returned for non-2xx responses.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// RPCError is a JSON-RPC error object returned by the server.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// InitializeResult is the subset of the initialize result mcp-slo cares about.
type InitializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Call sends a JSON-RPC request and waits for its response.
func (c *Client) Call(ctx context.Context, s *Session, method string, params any) (*Response, error) {
	id := nextID.Add(1)
	return c.send(ctx, s, request{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
}

// Notify sends a JSON-RPC notification. Servers answer with 202 Accepted.
func (c *Client) Notify(ctx context.Context, s *Session, method string, params any) (*Response, error) {
	return c.send(ctx, s, request{JSONRPC: "2.0", Method: method, Params: params})
}

// Initialize opens a new session at the requested protocol version.
func (c *Client) Initialize(ctx context.Context, version string) (*Session, *InitializeResult, *Response, error) {
	params := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcp-slo", "version": ClientVersion},
	}
	resp, err := c.Call(ctx, nil, "initialize", params)
	if err != nil {
		return nil, nil, resp, err
	}
	var res InitializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, nil, resp, fmt.Errorf("decode initialize result: %w", err)
	}
	if res.ProtocolVersion == "" {
		return nil, nil, resp, errors.New("initialize result has no protocolVersion")
	}
	s := &Session{ID: resp.Header.Get("Mcp-Session-Id"), ProtocolVersion: res.ProtocolVersion}
	return s, &res, resp, nil
}

// Close terminates a stateful session with DELETE. Errors are ignored: many
// servers are stateless and answer 405.
func (c *Client) Close(s *Session) {
	if s == nil || s.ID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.URL, nil)
	if err != nil {
		return
	}
	c.setHeaders(req, s)
	if resp, err := c.httpClient().Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
	}
}

func (c *Client) setHeaders(req *http.Request, s *Session) {
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if s != nil {
		if s.ID != "" {
			req.Header.Set("Mcp-Session-Id", s.ID)
		}
		if s.ProtocolVersion != "" {
			req.Header.Set("MCP-Protocol-Version", s.ProtocolVersion)
		}
	}
}

func (c *Client) send(ctx context.Context, s *Session, msg request) (*Response, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, s)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := &Response{Status: resp.StatusCode, Header: resp.Header}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return out, &HTTPError{Status: resp.StatusCode, Body: truncate(strings.TrimSpace(string(b)), 200)}
	}
	if msg.ID == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return out, nil
	}

	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var rpc *response
	switch ct {
	case "text/event-stream":
		rpc, err = readSSE(resp.Body, *msg.ID)
	case "application/json", "":
		rpc, err = readJSON(resp.Body, *msg.ID)
	default:
		err = fmt.Errorf("unexpected Content-Type %q", ct)
	}
	if err != nil {
		return out, err
	}
	if rpc.Error != nil {
		return out, rpc.Error
	}
	out.Result = rpc.Result
	return out, nil
}

func readJSON(r io.Reader, id int64) (*response, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	var msgs []response
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("decode JSON-RPC batch: %w", err)
		}
	} else {
		var m response
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("decode JSON-RPC response: %w (body: %s)", err, truncate(string(raw), 120))
		}
		msgs = append(msgs, m)
	}
	for i := range msgs {
		if isResponseTo(&msgs[i], id) {
			return &msgs[i], nil
		}
	}
	return nil, fmt.Errorf("no response with id %d in JSON body", id)
}

// readSSE scans an event stream until it sees the response to id. Server
// notifications and requests that arrive first are skipped.
func readSSE(r io.Reader, id int64) (*response, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	var data strings.Builder
	dispatch := func() *response {
		defer data.Reset()
		var m response
		if json.Unmarshal([]byte(data.String()), &m) == nil && isResponseTo(&m, id) {
			return &m
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if data.Len() > 0 {
				if m := dispatch(); m != nil {
					return m, nil
				}
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field == "data" {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(value, " "))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}
	if data.Len() > 0 {
		if m := dispatch(); m != nil {
			return m, nil
		}
	}
	return nil, fmt.Errorf("SSE stream ended without a response to id %d", id)
}

func isResponseTo(m *response, id int64) bool {
	if m.Method != "" || (m.Result == nil && m.Error == nil) {
		return false
	}
	return strings.Trim(string(m.ID), `"`) == strconv.FormatInt(id, 10)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
