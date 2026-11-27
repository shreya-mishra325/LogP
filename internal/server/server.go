package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"browsercompute.local/runtime/internal/engine"
	"browsercompute.local/runtime/internal/identity"
	"browsercompute.local/runtime/internal/protocol"
	"browsercompute.local/runtime/internal/web"
	"browsercompute.local/runtime/internal/workload"
)

type Server struct {
	ctx              context.Context
	cancel           context.CancelFunc
	engine           *engine.Engine
	operator, worker [32]byte
	mux              *http.ServeMux
	unauthenticated  chan struct{}
	connections      chan struct{}
	api              chan struct{}
	mu               sync.Mutex
	closing          bool
	sockets          sync.WaitGroup
}

func New(ctx context.Context, operator, worker string, display identity.Display) (*Server, error) {
	if len(operator) == 0 || len(worker) == 0 || len(operator) > 512 || len(worker) > 512 ||
		strings.ContainsAny(operator+worker, " \t\r\n") || match(sha256.Sum256([]byte(operator)), worker) {
		return nil, errors.New("credentials must be distinct, nonempty and at most 512 bytes without whitespace")
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Server{
		ctx: ctx, cancel: cancel, engine: engine.New(ctx, nil, nil), mux: http.NewServeMux(),
		operator: sha256.Sum256([]byte(operator)), worker: sha256.Sum256([]byte(worker)),
		unauthenticated: make(chan struct{}, 8), connections: make(chan struct{}, engine.MaxWorkers+8),
		api: make(chan struct{}, 8),
	}
	s.mux.HandleFunc("GET /{$}", web.Page("operator.html", display))
	s.mux.HandleFunc("GET /worker", web.Page("worker.html", display))
	for _, path := range []string{"/worker.js", "/kernel.js", "/operator.js", "/style.css"} {
		s.mux.Handle("GET "+path, web.Assets())
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("GET /ws/v1/worker", s.websocket)
	s.mux.HandleFunc("POST /api/v1/jobs", s.authorize(s.submit))
	s.mux.HandleFunc("GET /api/v1/jobs/{id}", s.authorize(s.status))
	s.mux.HandleFunc("GET /api/v1/jobs/{id}/result", s.authorize(s.result))
	s.mux.HandleFunc("GET /api/v1/jobs/{id}/tiles", s.authorize(s.tiles))
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.authorize(s.cancelJob))
	s.mux.HandleFunc("GET /api/v1/workers", s.authorize(s.workers))
	return s, nil
}

func (s *Server) Close() {
	s.mu.Lock()
	s.closing = true
	s.cancel()
	s.mu.Unlock()
	s.sockets.Wait()
	<-s.engine.Done()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; worker-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	s.mux.ServeHTTP(w, r)
}

func match(expected [32]byte, token string) bool {
	actual := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(expected[:], actual[:]) == 1
}

func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if values := r.Header.Values("Origin"); len(values) > 0 && (len(values) != 1 || values[0] != origin(r)) {
			problem(w, 403, "origin rejected")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !match(s.operator, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem(w, 401, "operator credential required")
			return
		}
		select {
		case s.api <- struct{}{}:
			defer func() { <-s.api }()
		default:
			problem(w, 429, "too many API requests")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func problem(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func engineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, engine.ErrMissing):
		problem(w, 404, err.Error())
	case errors.Is(err, engine.ErrConflict):
		problem(w, 409, err.Error())
	case errors.Is(err, engine.ErrCapacity):
		problem(w, 429, err.Error())
	default:
		problem(w, 503, "coordinator unavailable")
	}
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		problem(w, 400, "invalid or oversized job body")
		return
	}
	var in workload.Input
	// Pointers detect missing or null coordinates, including valid zero.
	var fields struct {
		workload.Input
		XMin *float64 `json:"xmin"`
		XMax *float64 `json:"xmax"`
		YMin *float64 `json:"ymin"`
		YMax *float64 `json:"ymax"`
	}
	if protocol.Decode(data, &fields) != nil || fields.XMin == nil || fields.XMax == nil || fields.YMin == nil || fields.YMax == nil {
		problem(w, 400, "invalid job object")
		return
	}
	in = fields.Input
	in.XMin, in.XMax, in.YMin, in.YMax = *fields.XMin, *fields.XMax, *fields.YMin, *fields.YMax
	if err := in.Validate(); err != nil {
		problem(w, 400, err.Error())
		return
	}
	v, err := s.engine.Submit(r.Context(), in)
	if err != nil {
		engineError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+v.ID)
	writeJSON(w, 201, v)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	v, err := s.engine.Status(r.Context(), r.PathValue("id"))
	if err != nil {
		engineError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	v, err := s.engine.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		engineError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) tiles(w http.ResponseWriter, r *http.Request) {
	cursor := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		var err error
		cursor, err = strconv.Atoi(raw)
		if err != nil || cursor < 0 {
			problem(w, 400, "invalid cursor")
			return
		}
	}
	v, err := s.engine.Tiles(r.Context(), r.PathValue("id"), cursor)
	if err != nil {
		engineError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) result(w http.ResponseWriter, r *http.Request) {
	in, tiles, err := s.engine.Result(r.Context(), r.PathValue("id"))
	if err != nil {
		engineError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_ = workload.PNG(w, in, tiles)
}

func (s *Server) workers(w http.ResponseWriter, r *http.Request) {
	v, err := s.engine.Workers(r.Context())
	if err != nil {
		engineError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
