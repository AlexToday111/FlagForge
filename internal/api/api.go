package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flagforge/flagforge/internal/eventbus"
	"github.com/flagforge/flagforge/internal/observability"
	"github.com/flagforge/flagforge/internal/repository"
	"github.com/flagforge/flagforge/internal/streaming"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type Server struct {
	Repo      *repository.Repository
	Bus       *eventbus.Redis
	Hub       *streaming.Hub
	Metrics   *observability.Metrics
	Heartbeat time.Duration
	CacheTTL  time.Duration
	Context   context.Context
	limiter   bucket
}
type principalKey struct{}

func principal(r *http.Request) repository.Principal {
	return r.Context().Value(principalKey{}).(repository.Principal)
}

type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.last.IsZero() {
		b.tokens = 200
	} else {
		b.tokens += now.Sub(b.last).Seconds() * 100
		if b.tokens > 200 {
			b.tokens = 200
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
func failure(w http.ResponseWriter, err error) {
	status, code, message := http.StatusInternalServerError, "internal_error", "internal server error"
	switch {
	case errors.Is(err, repository.ErrNotFound):
		status, code, message = 404, "not_found", err.Error()
	case errors.Is(err, repository.ErrConflict):
		status, code, message = 409, "version_conflict", err.Error()
	case errors.Is(err, repository.ErrInvalid):
		status, code, message = 400, "invalid_configuration", err.Error()
	case errors.Is(err, repository.ErrForbidden):
		status, code, message = 403, "forbidden", err.Error()
	default:
		slog.Error("request failed", "error", err)
	}
	write(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		failure(w, fmt.Errorf("%w: invalid JSON body", repository.ErrInvalid))
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		failure(w, fmt.Errorf("%w: expected one JSON value", repository.ErrInvalid))
		return false
	}
	return true
}
func expected(w http.ResponseWriter, r *http.Request) (int64, bool) {
	value := strings.Trim(r.Header.Get("If-Match"), `"`)
	if value == "" {
		write(w, 428, map[string]any{"error": map[string]string{"code": "precondition_required", "message": "If-Match version required (0 for new config)"}})
		return 0, false
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		failure(w, repository.ErrInvalid)
		return 0, false
	}
	return n, true
}

func (s *Server) auth(sdk bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sdk && !s.limiter.allow() {
			w.Header().Set("Retry-After", "1")
			write(w, 429, map[string]string{"error": "rate limit exceeded"})
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" || key == r.Header.Get("Authorization") {
			write(w, 401, map[string]string{"error": "bearer token required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		p, err := s.Repo.Authenticate(ctx, key, sdk)
		cancel()
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				write(w, 401, map[string]string{"error": "invalid or revoked token"})
			} else {
				failure(w, err)
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
		r.ResponseWriter.WriteHeader(status)
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(200)
	}
	return r.ResponseWriter.Write(b)
}
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) Handler(profiling bool) http.Handler {
	admin := http.NewServeMux()
	sdk := http.NewServeMux()
	root := http.NewServeMux()
	admin.HandleFunc("POST /api/v1/projects", s.createProject)
	admin.HandleFunc("GET /api/v1/projects", s.projects)
	admin.HandleFunc("POST /api/v1/projects/{project}/environments", s.createEnvironment)
	admin.HandleFunc("GET /api/v1/projects/{project}/environments", s.environments)
	admin.HandleFunc("POST /api/v1/projects/{project}/flags", s.createFlag)
	admin.HandleFunc("GET /api/v1/projects/{project}/flags", s.flags)
	admin.HandleFunc("GET /api/v1/projects/{project}/flags/{flag}", s.flag)
	admin.HandleFunc("PATCH /api/v1/projects/{project}/flags/{flag}", s.patchFlag)
	admin.HandleFunc("DELETE /api/v1/projects/{project}/flags/{flag}", s.archiveFlag)
	base := "/api/v1/projects/{project}/environments/{env}"
	admin.HandleFunc("GET "+base+"/flags/{flag}", s.getConfig)
	admin.HandleFunc("PUT "+base+"/flags/{flag}", s.putConfig)
	admin.HandleFunc("POST "+base+"/flags/{flag}/enable", s.enable)
	admin.HandleFunc("POST "+base+"/flags/{flag}/disable", s.disable)
	admin.HandleFunc("POST "+base+"/flags/{flag}/rollback/{version}", s.rollback)
	admin.HandleFunc("GET "+base+"/snapshot", s.adminSnapshot)
	admin.HandleFunc("PUT "+base+"/segments/{segment}", s.putSegment)
	admin.HandleFunc("POST "+base+"/sdk-keys", s.rotateKey)
	admin.HandleFunc("GET "+base+"/sdk-keys", s.listKeys)
	admin.HandleFunc("DELETE "+base+"/sdk-keys/{id}", s.revokeKey)
	admin.HandleFunc("POST "+base+"/flags/{flag}/schedules", s.schedule)
	admin.HandleFunc("POST "+base+"/flags/{flag}/gradual-rollout", s.gradual)
	admin.HandleFunc("GET "+base+"/schedules", s.schedules)
	admin.HandleFunc("DELETE "+base+"/schedules/{id}", s.cancelSchedule)
	admin.HandleFunc("GET /api/v1/audit", s.audit)
	admin.HandleFunc("GET /api/v1/stale-flags", s.stale)
	admin.HandleFunc("GET /api/v1/usage", s.usage)
	admin.HandleFunc("POST /api/v1/tokens", s.createToken)
	admin.HandleFunc("DELETE /api/v1/tokens/{id}", s.revokeToken)
	sdk.HandleFunc("GET /sdk/v1/config", s.snapshot)
	sdk.HandleFunc("GET /sdk/v1/stream", s.stream)
	sdk.HandleFunc("POST /sdk/v1/events/batch", s.events)
	root.Handle("/api/", s.auth(false, admin))
	root.Handle("/sdk/", s.auth(true, sdk))
	root.Handle("GET /metrics", promhttp.HandlerFor(s.Metrics.Registry, promhttp.HandlerOpts{}))
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	root.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.Repo.Pool.Ping(ctx) != nil {
			write(w, 503, map[string]string{"status": "database unavailable"})
			return
		}
		write(w, 200, map[string]string{"status": "ready"})
	})
	if profiling {
		profiles := http.NewServeMux()
		profiles.HandleFunc("/debug/pprof/", pprof.Index)
		profiles.HandleFunc("/debug/pprof/profile", pprof.Profile)
		profiles.HandleFunc("/debug/pprof/trace", pprof.Trace)
		profiles.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		root.Handle("/debug/pprof/", s.auth(false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if principal(r).Role != "Admin" {
				failure(w, repository.ErrForbidden)
				return
			}
			profiles.ServeHTTP(w, r)
		})))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, span := otel.Tracer("flagforge/api").Start(r.Context(), "http.request")
		defer span.End()
		wrapped := &recorder{ResponseWriter: w}
		request := r.WithContext(ctx)
		root.ServeHTTP(wrapped, request)
		route := request.Pattern
		// Mounted paths intentionally aggregate API families into bounded labels.
		if route == "" {
			route = "unmatched"
		}
		if wrapped.status == 0 {
			wrapped.status = 200
		}
		s.Metrics.Requests.WithLabelValues(route, strconv.Itoa(wrapped.status)).Inc()
		s.Metrics.Duration.WithLabelValues(route).Observe(time.Since(start).Seconds())
		span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", wrapped.status))
	})
}
