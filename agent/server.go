package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/episode"
	rt "github.com/xartus2004/arena/runtime"
)

// CreateRequest is the body of POST /episodes.
type CreateRequest struct {
	Env  string `json:"env"`
	Task string `json:"task"`
	// ID makes creation idempotent: re-POSTing an ID returns the existing
	// episode, or starts a new attempt if the last one failed for
	// infrastructure reasons.
	ID string `json:"id,omitempty"`
}

// EpisodeView is the body of GET /episodes/{id}.
type EpisodeView struct {
	ID          string               `json:"id"`
	Env         string               `json:"env"`
	Task        string               `json:"task"`
	Attempt     int                  `json:"attempt"`
	State       episode.State        `json:"state"`
	Deadline    time.Time            `json:"deadline"`
	Transitions []episode.Transition `json:"transitions"`
	Outcome     *episode.Outcome     `json:"outcome,omitempty"`
}

// apiError is the JSON error body.
type apiError struct {
	Error string `json:"error"`
	Infra bool   `json:"infra,omitempty"`
}

// Admission limits concurrent episodes (implemented by the scheduler).
type Admission interface {
	// TryAcquire reserves a slot or returns false when saturated.
	TryAcquire() bool
	Release()
}

// ServerConfig configures the API server.
type ServerConfig struct {
	Envs        map[string]*env.Environment
	Episode     episode.Config
	MaxAttempts int
	// Admission bounds concurrent live episodes (optional).
	Admission Admission
	// OnOutcome is called once per terminal attempt (optional; e.g. store).
	OnOutcome func(*episode.Outcome)
	Logger    *slog.Logger
	// WatchdogInterval controls how often idle episodes past their deadline
	// are force-submitted (default 1s).
	WatchdogInterval time.Duration
}

type entry struct {
	mu   sync.Mutex // serialises create/retry for an ID
	ep   *episode.Episode
	held bool // holds an admission slot
}

// Server serves the episode API.
type Server struct {
	cfg ServerConfig
	mux *http.ServeMux

	mu       sync.Mutex
	episodes map[string]*entry

	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewServer builds the server and starts its watchdog. Call Close to stop.
func NewServer(cfg ServerConfig) *Server {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 3
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.WatchdogInterval <= 0 {
		cfg.WatchdogInterval = time.Second
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), episodes: map[string]*entry{}, stop: make(chan struct{})}
	s.mux.HandleFunc("POST /episodes", s.handleCreate)
	s.mux.HandleFunc("POST /episodes/{id}/step", s.handleStep)
	s.mux.HandleFunc("GET /episodes/{id}", s.handleGet)
	s.mux.HandleFunc("GET /envs", s.handleEnvs)
	s.wg.Add(1)
	go s.watchdog()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Close stops the watchdog and aborts every live episode, destroying their
// sandboxes.
func (s *Server) Close(ctx context.Context) {
	s.closeOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
	s.mu.Lock()
	entries := make([]*entry, 0, len(s.episodes))
	for _, e := range s.episodes {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		if e.ep != nil && e.ep.Outcome() == nil {
			e.ep.Abort(ctx, rt.Infra("server shutdown", errors.New("server closed")))
			s.finished(e)
		}
		if e.ep != nil {
			e.ep.Close(ctx)
		}
		e.mu.Unlock()
	}
}

func (s *Server) watchdog() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.WatchdogInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		entries := make([]*entry, 0, len(s.episodes))
		for _, e := range s.episodes {
			entries = append(entries, e)
		}
		s.mu.Unlock()
		for _, e := range entries {
			if !e.mu.TryLock() {
				continue
			}
			if e.ep != nil {
				if expired, _ := e.ep.Expire(context.Background()); expired {
					s.finished(e)
				}
			}
			e.mu.Unlock()
		}
	}
}

// finished releases resources once an attempt is terminal (e.mu held).
func (s *Server) finished(e *entry) {
	o := e.ep.Outcome()
	if o == nil {
		return
	}
	if e.held && s.cfg.Admission != nil {
		s.cfg.Admission.Release()
	}
	e.held = false
	if s.cfg.OnOutcome != nil {
		s.cfg.OnOutcome(o)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, apiError{Error: err.Error(), Infra: rt.IsInfra(err)})
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ep-" + hex.EncodeToString(b)
}

func (s *Server) handleEnvs(w http.ResponseWriter, _ *http.Request) {
	out := map[string][]string{}
	for name, e := range s.cfg.Envs {
		for _, t := range e.Tasks {
			out[name] = append(out[name], t.ID)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}
	e, ok := s.cfg.Envs[req.Env]
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown env %q", req.Env))
		return
	}
	task, err := e.Task(req.Task)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if req.ID == "" {
		req.ID = newID()
	}
	if strings.ContainsAny(req.ID, "/\\ ") || len(req.ID) > 128 {
		writeErr(w, http.StatusBadRequest, errors.New("invalid id"))
		return
	}
	s.mu.Lock()
	ent, exists := s.episodes[req.ID]
	if !exists {
		ent = &entry{}
		s.episodes[req.ID] = ent
	}
	s.mu.Unlock()

	ent.mu.Lock()
	defer ent.mu.Unlock()
	attempt := 1
	if ent.ep != nil {
		o := ent.ep.Outcome()
		if ent.ep.Task() != task {
			writeErr(w, http.StatusConflict, errors.New("id already used for a different task"))
			return
		}
		retryable := o != nil && o.Status == episode.StatusFailed && o.Failure == episode.FailureInfra
		if !retryable || ent.ep.Attempt >= s.cfg.MaxAttempts {
			writeJSON(w, http.StatusOK, s.view(ent.ep, nil))
			return
		}
		attempt = ent.ep.Attempt + 1
	}
	if s.cfg.Admission != nil {
		if !s.cfg.Admission.TryAcquire() {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, errors.New("at capacity; retry later"))
			return
		}
		ent.held = true
	}
	ep := episode.New(s.cfg.Episode, task, req.ID, attempt)
	ent.ep = ep
	obs, err := ep.Start(r.Context())
	if err != nil {
		s.finished(ent)
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.view(ep, &obs))
}

// StepResponse is the body of POST /episodes/{id}/step.
type StepResponse struct {
	Observation episode.Observation `json:"observation"`
}

func (s *Server) lookup(id string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.episodes[id]
}

func (s *Server) handleStep(w http.ResponseWriter, r *http.Request) {
	ent := s.lookup(r.PathValue("id"))
	if ent == nil {
		writeErr(w, http.StatusNotFound, errors.New("no such episode"))
		return
	}
	var call episode.ToolCall
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&call); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()
	if ent.ep == nil {
		writeErr(w, http.StatusNotFound, errors.New("no such episode"))
		return
	}
	// Detach from the request context: a client disconnect must not leave
	// custody half-done.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()
	obs, err := ent.ep.Step(ctx, call)
	if obs.Done || ent.ep.Outcome() != nil {
		s.finished(ent)
	}
	switch {
	case errors.Is(err, episode.ErrNotStudentTurn):
		writeErr(w, http.StatusConflict, err)
	case err != nil:
		writeErr(w, http.StatusServiceUnavailable, err)
	default:
		writeJSON(w, http.StatusOK, StepResponse{Observation: obs})
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	ent := s.lookup(r.PathValue("id"))
	if ent == nil {
		writeErr(w, http.StatusNotFound, errors.New("no such episode"))
		return
	}
	ent.mu.Lock()
	ep := ent.ep
	ent.mu.Unlock()
	if ep == nil {
		writeErr(w, http.StatusNotFound, errors.New("no such episode"))
		return
	}
	writeJSON(w, http.StatusOK, s.view(ep, nil))
}

// CreateResponse is the body returned by POST /episodes.
type CreateResponse struct {
	EpisodeView
	Observation *episode.Observation `json:"observation,omitempty"`
}

func (s *Server) view(ep *episode.Episode, obs *episode.Observation) CreateResponse {
	return CreateResponse{
		EpisodeView: EpisodeView{
			ID: ep.ID, Env: ep.Task().Env().Name, Task: ep.Task().ID, Attempt: ep.Attempt,
			State: ep.State(), Deadline: ep.Deadline(), Transitions: ep.Transitions(), Outcome: ep.Outcome(),
		},
		Observation: obs,
	}
}
