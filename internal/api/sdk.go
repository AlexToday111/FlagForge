package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/flagforge/flagforge/pkg/types"
	"net/http"
	"time"
)

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	envID := principal(r).EnvironmentID
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var version int64
	if err := s.Repo.Pool.QueryRow(ctx, `SELECT version FROM environments WHERE id=$1`, envID).Scan(&version); err != nil {
		failure(w, err)
		return
	}
	etag := fmt.Sprintf(`"%d"`, version)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	var snapshot types.Snapshot
	cached := false
	if s.Bus != nil {
		cacheCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		snapshot, cached = s.Bus.GetSnapshot(cacheCtx, envID, version)
		stop()
	}
	if !cached {
		var err error
		snapshot, err = s.Repo.Snapshot(ctx, envID)
		if err != nil {
			failure(w, err)
			return
		}
		if s.Bus != nil {
			cacheCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
			_ = s.Bus.CacheSnapshot(cacheCtx, envID, snapshot, s.CacheTTL)
			stop()
		}
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, snapshot.Version))
	write(w, 200, snapshot)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	envID := principal(r).EnvironmentID
	ch, unsubscribe := s.Hub.Subscribe(envID)
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		failure(w, err)
		return
	}
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	if err := controller.Flush(); err != nil {
		return
	}
	s.Metrics.Connections.Inc()
	defer s.Metrics.Connections.Dec()
	ticker := time.NewTicker(s.Heartbeat)
	defer ticker.Stop()
	key := r.Header.Get("Authorization")[7:]
	var shutdown <-chan struct{}
	if s.Context != nil {
		shutdown = s.Context.Done()
	}
	for {
		var e types.Event
		send := false
		select {
		case <-r.Context().Done():
			return
		case <-shutdown:
			return
		case e = <-ch:
			send = true
		case <-ticker.C:
			// Revalidate revoked keys and periodically reconcile independently of Redis.
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			_, err := s.Repo.Authenticate(ctx, key, true)
			if err == nil {
				err = s.Repo.Pool.QueryRow(ctx, `SELECT version FROM environments WHERE id=$1`, envID).Scan(&e.Version)
				e.EnvironmentID = envID
				send = err == nil
			}
			cancel()
			if err != nil {
				return
			}
		}
		if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return
		}
		if send {
			b, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: flag-update\ndata: %s\n\n", e.Version, b); err != nil {
				s.Metrics.DeliveryFailures.Inc()
				return
			}
			s.Metrics.StreamingUpdates.Inc()
			if !e.CreatedAt.IsZero() {
				latency := time.Since(e.CreatedAt).Seconds()
				if latency >= 0 {
					s.Metrics.Propagation.Observe(latency)
				}
			}
		}
		if err := controller.Flush(); err != nil {
			s.Metrics.DeliveryFailures.Inc()
			return
		}
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Events []types.EvaluationEvent `json:"events"`
	}
	if !decode(w, r, &input) {
		return
	}
	if err := s.Repo.RecordEvents(r.Context(), principal(r).EnvironmentID, input.Events); err != nil {
		failure(w, err)
		return
	}
	for _, e := range input.Events {
		switch e.Reason {
		case "DEFAULT", "ERROR", "FLAG_DISABLED", "TARGET_MATCH", "RULE_MATCH", "ROLLOUT", "PREREQUISITE_FAILED":
			s.Metrics.Evaluations.WithLabelValues(e.Reason).Inc()
		}
	}
	write(w, 202, map[string]int{"accepted": len(input.Events)})
}
