package runner

import (
	"math"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Duration marshals to JSON as fractional milliseconds.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	ms, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return err
	}
	*d = Duration(ms * float64(time.Millisecond))
	return nil
}

// MethodStats summarizes the latency and errors of one JSON-RPC method.
// Latency percentiles cover successful requests only, so fast rejections such
// as 429s cannot make a server look quicker than it is.
type MethodStats struct {
	Method     string   `json:"method"`
	Count      int      `json:"count"`
	Errors     int      `json:"errors"`
	ErrorRate  float64  `json:"error_rate"`
	P50        Duration `json:"p50_ms"`
	P90        Duration `json:"p90_ms"`
	P95        Duration `json:"p95_ms"`
	P99        Duration `json:"p99_ms"`
	Max        Duration `json:"max_ms"`
	Mean       Duration `json:"mean_ms"`
	FirstError string   `json:"first_error,omitempty"`
}

// methodOrder is the order methods are reported in.
var methodOrder = []string{"initialize", "notifications/initialized", "tools/list", "tools/call"}

// Recorder collects samples from concurrent workers.
type Recorder struct {
	mu         sync.Mutex
	samples    map[string][]time.Duration // successful requests
	errors     map[string]int
	firstErr   map[string]string
	firstError string
}

func NewRecorder() *Recorder {
	return &Recorder{
		samples:  map[string][]time.Duration{},
		errors:   map[string]int{},
		firstErr: map[string]string{},
	}
}

func (r *Recorder) Record(method string, d time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.samples[method]; !ok {
		r.samples[method] = nil
	}
	if err == nil {
		r.samples[method] = append(r.samples[method], d)
	} else {
		r.errors[method]++
		if _, ok := r.firstErr[method]; !ok {
			r.firstErr[method] = err.Error()
		}
		if r.firstError == "" {
			r.firstError = method + ": " + err.Error()
		}
	}
}

// Summary returns per-method stats (in protocol order) and the aggregate over
// every request.
func (r *Recorder) Summary() (methods []MethodStats, overall MethodStats, firstError string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []time.Duration
	errs := 0
	for _, m := range methodOrder {
		s, ok := r.samples[m]
		if !ok {
			continue
		}
		methods = append(methods, summarize(m, s, r.errors[m], r.firstErr[m]))
		all = append(all, s...)
		errs += r.errors[m]
	}
	overall = summarize("all", all, errs, r.firstError)
	return methods, overall, r.firstError
}

func summarize(method string, samples []time.Duration, errs int, firstErr string) MethodStats {
	st := MethodStats{Method: method, Count: len(samples) + errs, Errors: errs, FirstError: firstErr}
	if st.Count > 0 {
		st.ErrorRate = float64(errs) / float64(st.Count)
	}
	if len(samples) == 0 {
		return st
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	st.P50 = Duration(percentile(sorted, 50))
	st.P90 = Duration(percentile(sorted, 90))
	st.P95 = Duration(percentile(sorted, 95))
	st.P99 = Duration(percentile(sorted, 99))
	st.Max = Duration(sorted[len(sorted)-1])
	st.Mean = Duration(sum / time.Duration(len(sorted)))
	return st
}

// percentile uses the nearest-rank method on sorted samples.
func percentile(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}
