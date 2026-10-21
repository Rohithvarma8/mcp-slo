package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Rohithvarma8/mcp-slo/internal/mcp"
)

// Negotiation is one row of the protocol negotiation matrix.
type Negotiation struct {
	Requested  string `json:"requested"`
	Status     int    `json:"http_status"`
	Negotiated string `json:"negotiated,omitempty"`
	Result     string `json:"result"` // accepted, downgraded, rejected
	Error      string `json:"error,omitempty"`
}

func negotiate(ctx context.Context, c *mcp.Client, versions []string) []Negotiation {
	var out []Negotiation
	for _, v := range versions {
		s, res, resp, err := c.Initialize(ctx, v)
		n := Negotiation{Requested: v}
		if resp != nil {
			n.Status = resp.Status
		}
		switch {
		case err != nil:
			n.Result, n.Error = "rejected", err.Error()
		case res.ProtocolVersion == v:
			n.Result, n.Negotiated = "accepted", res.ProtocolVersion
		default:
			n.Result, n.Negotiated = "downgraded", res.ProtocolVersion
		}
		c.Close(s)
		out = append(out, n)
	}
	return out
}

// loadVersion picks the protocol version for the load test: the newest one the
// server accepted outright, else the newest it negotiated down to.
func loadVersion(matrix []Negotiation) string {
	for i := len(matrix) - 1; i >= 0; i-- {
		if matrix[i].Result == "accepted" {
			return matrix[i].Requested
		}
	}
	for i := len(matrix) - 1; i >= 0; i-- {
		if matrix[i].Negotiated != "" {
			return matrix[i].Negotiated
		}
	}
	return "2025-06-18"
}

// AuthReport describes how the server treats missing or bad credentials.
type AuthReport struct {
	Required bool        `json:"required"`
	Summary  string      `json:"summary"`
	Probes   []AuthProbe `json:"probes,omitempty"`
}

type AuthProbe struct {
	Name            string `json:"name"`
	Status          int    `json:"http_status"`
	WWWAuthenticate bool   `json:"www_authenticate"`
	Passed          bool   `json:"passed"`
	Detail          string `json:"detail"`
}

// expiredJWT is a structurally valid JWT whose exp is 1970-01-01T00:00:01Z.
var expiredJWT = func() string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		enc([]byte(`{"sub":"mcp-slo","iat":0,"exp":1}`)) + "." +
		enc([]byte("mcp-slo-not-a-real-signature"))
}()

func probeAuth(ctx context.Context, c *mcp.Client, profile string) AuthReport {
	probes := []struct{ name, auth string }{
		{"missing token", ""},
		{"malformed token", "Bearer not a valid token!!"},
		{"expired token", "Bearer " + expiredJWT},
	}
	var rep AuthReport
	for i, p := range probes {
		pc := *c
		pc.Header = c.Header.Clone()
		if pc.Header == nil {
			pc.Header = http.Header{}
		}
		pc.Header.Del("Authorization")
		if p.auth != "" {
			pc.Header.Set("Authorization", p.auth)
		}
		s, _, resp, err := pc.Initialize(ctx, mcp.KnownProtocolVersions[1])
		pc.Close(s)

		if i == 0 && err == nil {
			rep.Summary = "no auth required (initialize without a token succeeded); auth probes skipped"
			return rep
		}
		if i == 0 && resp == nil {
			rep.Summary = "auth probes skipped: server unreachable (" + err.Error() + ")"
			return rep
		}
		rep.Required = true
		pr := AuthProbe{Name: p.name}
		if resp == nil {
			pr.Detail = "request failed: " + err.Error()
			rep.Probes = append(rep.Probes, pr)
			continue
		}
		pr.Status = resp.Status
		pr.WWWAuthenticate = resp.Header.Get("WWW-Authenticate") != ""
		pr.Passed, pr.Detail = judgeAuth(pr, err, profile)
		rep.Probes = append(rep.Probes, pr)
	}
	rep.Summary = "auth required"
	return rep
}

func judgeAuth(p AuthProbe, err error, profile string) (bool, string) {
	switch {
	case err == nil:
		return false, "server accepted the request: invalid credentials must be rejected"
	case profile == "alexa" && p.Status == http.StatusUnauthorized && p.WWWAuthenticate:
		return false, "alexa: 401 must not carry a WWW-Authenticate header"
	case profile == "alexa" && p.Status != http.StatusUnauthorized:
		return false, fmt.Sprintf("alexa: expected 401, got %d", p.Status)
	case p.Status == http.StatusUnauthorized || p.Status == http.StatusForbidden:
		if p.WWWAuthenticate {
			return true, fmt.Sprintf("rejected with %d and a WWW-Authenticate challenge", p.Status)
		}
		return true, fmt.Sprintf("rejected with %d, no WWW-Authenticate", p.Status)
	default:
		return false, fmt.Sprintf("expected 401, got %d (%v)", p.Status, err)
	}
}

// Check is a named pass/fail result outside the SLOs.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// checkGET verifies a GET on the endpoint completes instead of holding an SSE
// stream open, which Alexa+ treats as a hang.
func checkGET(ctx context.Context, c *mcp.Client, timeout time.Duration) Check {
	const name = "alexa: GET does not hang"
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return Check{Name: name, Detail: err.Error()}
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "text/event-stream")
	// Use a client without its own timeout so ctx decides.
	hc := &http.Client{Transport: http.DefaultTransport}
	if c.HTTP != nil && c.HTTP.Transport != nil {
		hc.Transport = c.HTTP.Transport
	}
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Check{Name: name, Detail: fmt.Sprintf("no response headers within %s", timeout)}
		}
		return Check{Name: name, Detail: "GET failed: " + err.Error()}
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Check{Name: name, Detail: fmt.Sprintf("GET returned %d (%s) and kept the stream open past %s; return 405 instead",
			resp.StatusCode, resp.Header.Get("Content-Type"), timeout)}
	}
	return Check{Name: name, Passed: true, Detail: fmt.Sprintf("GET returned %d and closed in %s", resp.StatusCode, FormatDuration(time.Since(start)))}
}
