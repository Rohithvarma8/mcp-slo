package runner

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SLO is a parsed objective such as "p95<500ms", "errors<1%" or
// "tools/call:p99<=1s". Without a method prefix it applies to all requests.
type SLO struct {
	Raw       string
	Method    string // "" means all requests
	Metric    string // p50, p90, p95, p99, max, mean, errors
	Inclusive bool   // <= instead of <
	Latency   time.Duration
	Rate      float64 // for errors, as a fraction
}

// SLOResult is the outcome of evaluating one SLO.
type SLOResult struct {
	SLO    string `json:"slo"`
	Scope  string `json:"scope"`
	Passed bool   `json:"passed"`
	Actual string `json:"actual"`
	Detail string `json:"detail,omitempty"`
}

var sloRe = regexp.MustCompile(`^(?:([\w/.-]+):)?(p50|p90|p95|p99|max|mean|errors)\s*(<=|<)\s*(\S+)$`)

func ParseSLO(s string) (SLO, error) {
	raw := strings.TrimSpace(s)
	m := sloRe.FindStringSubmatch(raw)
	if m == nil {
		return SLO{}, fmt.Errorf("invalid SLO %q: want e.g. p95<500ms, errors<1%% or tools/call:p99<1s", s)
	}
	slo := SLO{Raw: raw, Method: m[1], Metric: m[2], Inclusive: m[3] == "<="}
	if slo.Metric == "errors" {
		v, ok := strings.CutSuffix(m[4], "%")
		f, err := strconv.ParseFloat(v, 64)
		if !ok || err != nil || f < 0 || f > 100 {
			return SLO{}, fmt.Errorf("invalid SLO %q: error budget must be a percentage like 1%% or 0.5%%", s)
		}
		slo.Rate = f / 100
		return slo, nil
	}
	d, err := time.ParseDuration(m[4])
	if err != nil || d <= 0 {
		return SLO{}, fmt.Errorf("invalid SLO %q: latency must be a duration like 500ms or 1.5s", s)
	}
	slo.Latency = d
	return slo, nil
}

// Evaluate checks each SLO against the measured stats.
func Evaluate(slos []SLO, methods []MethodStats, overall MethodStats) []SLOResult {
	var out []SLOResult
	for _, slo := range slos {
		st, scope := overall, "all requests"
		if slo.Method != "" {
			scope = slo.Method
			st = MethodStats{Method: slo.Method}
			for _, m := range methods {
				if m.Method == slo.Method {
					st = m
				}
			}
		}
		r := SLOResult{SLO: slo.Raw, Scope: scope}
		if st.Count == 0 {
			r.Actual = "no samples"
			r.Detail = "no requests were recorded for " + scope
			out = append(out, r)
			continue
		}
		if slo.Metric == "errors" {
			r.Actual = FormatPercent(st.ErrorRate)
			r.Passed = st.ErrorRate < slo.Rate || (slo.Inclusive && st.ErrorRate == slo.Rate)
			if !r.Passed {
				r.Detail = fmt.Sprintf("%d of %d requests failed", st.Errors, st.Count)
			}
		} else if st.Count == st.Errors {
			r.Actual = "no successful requests"
			r.Detail = "latency SLOs need at least one successful request"
		} else {
			v := time.Duration(pick(st, slo.Metric))
			r.Actual = FormatDuration(v)
			r.Passed = v < slo.Latency || (slo.Inclusive && v == slo.Latency)
			if !r.Passed {
				r.Detail = fmt.Sprintf("%s is %s, budget %s", slo.Metric, FormatDuration(v), FormatDuration(slo.Latency))
			}
		}
		out = append(out, r)
	}
	return out
}

func pick(st MethodStats, metric string) Duration {
	switch metric {
	case "p50":
		return st.P50
	case "p90":
		return st.P90
	case "p95":
		return st.P95
	case "p99":
		return st.P99
	case "mean":
		return st.Mean
	default:
		return st.Max
	}
}

// FormatDuration renders latencies compactly: 0.42ms, 87ms, 1.24s.
func FormatDuration(d time.Duration) string {
	switch {
	case d < 10*time.Millisecond:
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 2, 64) + "ms"
	case d < time.Second:
		return strconv.FormatInt(d.Round(time.Millisecond).Milliseconds(), 10) + "ms"
	default:
		return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) + "s"
	}
}

func FormatPercent(f float64) string {
	return strconv.FormatFloat(f*100, 'f', 2, 64) + "%"
}
