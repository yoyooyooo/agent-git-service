package edge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const requestIDHeader = "X-AGS-Edge-Request-ID"
const routeHeader = "X-AGS-Edge-Route"

type traceKey struct{}

// Only closed operational fields; never URL/query, credentials, bodies, user
// identity or raw errors. IDs are locally generated, not caller assertions.
type RequestEvent struct {
	ID         string    `json:"request_id"`
	At         time.Time `json:"at"`
	Route      string    `json:"route"`
	Stage      string    `json:"stage"`
	Code       string    `json:"code,omitempty"`
	Cache      string    `json:"cache,omitempty"`
	Status     int       `json:"status"`
	Bytes      int64     `json:"bytes"`
	DurationMS int64     `json:"duration_ms"`
	Aborted    bool      `json:"aborted"`
}
type requestTrace struct {
	mu    sync.Mutex
	event RequestEvent
}

func traceStage(ctx context.Context, stage string) {
	if t, _ := ctx.Value(traceKey{}).(*requestTrace); t != nil {
		t.mu.Lock()
		t.event.Stage = stage
		t.mu.Unlock()
	}
}
func traceCache(ctx context.Context, value string) {
	if t, _ := ctx.Value(traceKey{}).(*requestTrace); t != nil {
		t.mu.Lock()
		t.event.Cache = value
		t.mu.Unlock()
	}
}
func traceRoute(ctx context.Context, w http.ResponseWriter, route string) {
	w.Header().Set(routeHeader, route)
	if t, _ := ctx.Value(traceKey{}).(*requestTrace); t != nil {
		t.mu.Lock()
		t.event.Route = route
		t.mu.Unlock()
	}
}

type Telemetry struct {
	mu              sync.Mutex
	total, failures uint64
	byRoute         map[string]uint64
	recent          []RequestEvent
	active          map[string]*requestTrace
}
type TelemetrySnapshot struct {
	Requests uint64            `json:"requests"`
	Failures uint64            `json:"failures"`
	Routes   map[string]uint64 `json:"routes"`
	Recent   []RequestEvent    `json:"recent_requests"`
	Active   []RequestEvent    `json:"active_requests"`
}

func newTelemetry() *Telemetry {
	return &Telemetry{byRoute: make(map[string]uint64), active: make(map[string]*requestTrace)}
}
func (t *Telemetry) record(e RequestEvent) {
	t.mu.Lock()
	delete(t.active, e.ID)
	t.total++
	if e.Status >= 400 || e.Aborted {
		t.failures++
	}
	t.byRoute[e.Route]++
	if len(t.recent) == 64 {
		copy(t.recent, t.recent[1:])
		t.recent = t.recent[:63]
	}
	t.recent = append(t.recent, e)
	t.mu.Unlock()
	level := slog.LevelInfo
	if e.Status >= 400 || e.Aborted {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, "edge_request", "request_id", e.ID, "route", e.Route, "stage", e.Stage, "code", e.Code, "cache", e.Cache, "status", e.Status, "bytes", e.Bytes, "duration_ms", e.DurationMS, "aborted", e.Aborted)
}
func (t *Telemetry) Snapshot() TelemetrySnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := TelemetrySnapshot{Requests: t.total, Failures: t.failures, Routes: make(map[string]uint64), Recent: append([]RequestEvent{}, t.recent...), Active: []RequestEvent{}}
	for _, trace := range t.active {
		trace.mu.Lock()
		e := trace.event
		trace.mu.Unlock()
		e.DurationMS = time.Since(e.At).Milliseconds()
		s.Active = append(s.Active, e)
	}
	for k, v := range t.byRoute {
		s.Routes[k] = v
	}
	return s
}

type observedWriter struct {
	http.ResponseWriter
	trace *requestTrace
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) WriteHeader(status int) {
	w.trace.mu.Lock()
	if status >= 200 && w.trace.event.Status == 0 {
		w.trace.event.Status = status
	}
	w.trace.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedWriter) ensureStatus() {
	w.trace.mu.Lock()
	empty := w.trace.event.Status == 0
	w.trace.mu.Unlock()
	if empty {
		w.WriteHeader(http.StatusOK)
	}
}
func (w *observedWriter) Write(p []byte) (int, error) {
	w.ensureStatus()
	n, err := w.ResponseWriter.Write(p)
	w.trace.mu.Lock()
	w.trace.event.Bytes += int64(n)
	if err != nil {
		w.trace.event.Aborted = true
	}
	w.trace.mu.Unlock()
	return n, err
}
func (w *observedWriter) Flush() {
	w.ensureStatus()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *observedWriter) edgeFailure(code string) {
	w.trace.mu.Lock()
	w.trace.event.Code = code
	w.trace.mu.Unlock()
}
func recordFailure(w http.ResponseWriter, code string) {
	for {
		if recorder, ok := w.(interface{ edgeFailure(string) }); ok {
			recorder.edgeFailure(code)
			return
		}
		unwrap, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrap.Unwrap()
	}
}

func (s *Server) traceRequest(w http.ResponseWriter, r *http.Request) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		writeError(w, 503, "request_id_unavailable")
		return
	}
	started := time.Now()
	id := hex.EncodeToString(nonce[:])
	t := &requestTrace{event: RequestEvent{ID: id, At: started.UTC(), Route: "rejected", Stage: "routing"}}
	w.Header().Set(requestIDHeader, id)
	ctx := context.WithValue(r.Context(), traceKey{}, t)
	s.telemetry.mu.Lock()
	if len(s.telemetry.active) < 256 {
		s.telemetry.active[id] = t
	}
	s.telemetry.mu.Unlock()
	defer func() {
		p := recover()
		t.mu.Lock()
		if p != nil {
			t.event.Aborted = true
			if t.event.Code == "" {
				t.event.Code = "stream_aborted"
			}
		}
		if r.Context().Err() != nil {
			t.event.Aborted = true
			if t.event.Code == "" {
				t.event.Code = "client_cancelled"
			}
		}
		if t.event.Status == 0 {
			if t.event.Aborted {
				t.event.Status = 499
			} else {
				t.event.Status = 200
			}
		}
		t.event.DurationMS = time.Since(started).Milliseconds()
		event := t.event
		t.mu.Unlock()
		s.telemetry.record(event)
		if p != nil {
			panic(p)
		}
	}()
	s.serveRequest(&observedWriter{ResponseWriter: w, trace: t}, r.WithContext(ctx))
}
