//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/flagforge/flagforge/internal/api"
	"github.com/flagforge/flagforge/internal/eventbus"
	"github.com/flagforge/flagforge/internal/observability"
	"github.com/flagforge/flagforge/internal/repository"
	workers "github.com/flagforge/flagforge/internal/runtime"
	"github.com/flagforge/flagforge/internal/streaming"
	"github.com/flagforge/flagforge/migrations"
	"github.com/flagforge/flagforge/pkg/sdk"
	"github.com/flagforge/flagforge/pkg/types"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func infrastructure(t *testing.T) (string, string) {
	t.Helper()
	if dsn := os.Getenv("INTEGRATION_DATABASE_DSN"); dsn != "" {
		return dsn, os.Getenv("INTEGRATION_REDIS_ADDRESS")
	}
	ctx := context.Background()
	pg, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: testcontainers.ContainerRequest{Image: "postgres:16-alpine", Env: map[string]string{"POSTGRES_PASSWORD": "flagforge", "POSTGRES_USER": "flagforge", "POSTGRES_DB": "flagforge"}, ExposedPorts: []string{"5432/tcp"}, WaitingFor: wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(time.Minute)}, Started: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Terminate(ctx)) })
	host, err := pg.Host(ctx)
	require.NoError(t, err)
	port, err := pg.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	dsn := fmt.Sprintf("postgres://flagforge:flagforge@%s:%s/flagforge?sslmode=disable", host, port.Port())
	rd, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: testcontainers.ContainerRequest{Image: "redis:7-alpine", ExposedPorts: []string{"6379/tcp"}, WaitingFor: wait.ForListeningPort("6379/tcp")}, Started: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rd.Terminate(ctx)) })
	host, err = rd.Host(ctx)
	require.NoError(t, err)
	port, err = rd.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)
	return dsn, host + ":" + port.Port()
}

type failedPublisher struct{}

func (failedPublisher) Publish(context.Context, types.Event) error {
	return errors.New("redis unavailable")
}

func configuration() types.FlagConfig {
	return types.FlagConfig{Key: "new-checkout", Type: types.Boolean, Enabled: false, Variations: map[string]any{"on": true, "off": false}, DefaultVariation: "on", OffVariation: "off"}
}

func TestPlatform(t *testing.T) {
	dsn, redisAddress := infrastructure(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, migrations.Up(ctx, dsn))
	r, err := repository.New(ctx, dsn)
	require.NoError(t, err)
	defer r.Pool.Close()
	adminKey := "api_admin_integration_test_secret_123456"
	require.NoError(t, r.BootstrapAdmin(ctx, adminKey))
	p := repository.Principal{Actor: "test", Role: "Admin"}
	project := fmt.Sprintf("shop-%d", time.Now().UnixNano())
	_, err = r.CreateProject(ctx, p, project, "Shop")
	require.NoError(t, err)
	env, sdkKey, err := r.CreateEnvironment(ctx, p, project, types.Environment{Key: "production", Name: "Production"})
	require.NoError(t, err)
	_, err = r.CreateFlag(ctx, p, project, types.FeatureFlag{Key: "new-checkout", Name: "Checkout", Type: types.Boolean})
	require.NoError(t, err)
	f, err := r.UpdateConfig(ctx, p, project, "production", "new-checkout", 0, configuration())
	require.NoError(t, err)
	require.Equal(t, int64(1), f.Version)
	bus := eventbus.New(redisAddress, "")
	defer bus.Client.Close()
	t.Run("outbox failure and retry", func(t *testing.T) {
		_, err := r.PublishOutbox(ctx, failedPublisher{}, 100)
		require.Error(t, err)
		var pending int
		require.NoError(t, r.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND environment_id=$1`, env.ID).Scan(&pending))
		require.Equal(t, 1, pending)
		n, err := r.PublishOutbox(ctx, bus, 100)
		require.NoError(t, err)
		require.GreaterOrEqual(t, n, 1)
	})
	t.Run("concurrent config writes", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := r.UpdateConfig(ctx, p, project, "production", "new-checkout", 1, configuration())
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		success, conflicts := 0, 0
		for err := range errs {
			if err == nil {
				success++
			} else if errors.Is(err, repository.ErrConflict) {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
		require.Equal(t, 1, success)
		require.Equal(t, 1, conflicts)
	})
	hubA, hubB := streaming.New(), streaming.New()
	metricsA, metricsB := observability.New(), observability.New()
	joinA := workers.Start(ctx, r, bus, hubA, metricsA, 20*time.Millisecond, 100)
	joinB := workers.Start(ctx, r, bus, hubB, metricsB, 20*time.Millisecond, 100)
	defer func() { cancel(); joinA(); joinB() }()
	a := &api.Server{Repo: r, Bus: bus, Hub: hubA, Metrics: metricsA, Context: ctx, Heartbeat: 30 * time.Second, CacheTTL: time.Minute}
	b := &api.Server{Repo: r, Bus: bus, Hub: hubB, Metrics: metricsB, Context: ctx, Heartbeat: 30 * time.Second, CacheTTL: time.Minute}
	serverA, serverB := httptest.NewServer(a.Handler(true)), httptest.NewServer(b.Handler(false))
	defer serverA.Close()
	defer serverB.Close()
	call := func(method, path, key string, body any, version string) (int, []byte) {
		t.Helper()
		var content io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			require.NoError(t, err)
			content = bytes.NewReader(encoded)
		}
		req, err := http.NewRequest(method, serverA.URL+path, content)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+key)
		if version != "" {
			req.Header.Set("If-Match", version)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, data
	}
	base := "/api/v1/projects/" + project + "/environments/production/flags/new-checkout"
	t.Run("RBAC and HTTP preconditions", func(t *testing.T) {
		_, viewer, err := r.CreateToken(ctx, p, "reader", "Viewer")
		require.NoError(t, err)
		_, developer, err := r.CreateToken(ctx, p, "dev", "Developer")
		require.NoError(t, err)
		status, _ := call("GET", "/api/v1/projects", viewer, nil, "")
		require.Equal(t, 200, status)
		status, _ = call("PUT", base, viewer, configuration(), "2")
		require.Equal(t, 403, status)
		status, _ = call("PUT", base, developer, configuration(), "2")
		require.Equal(t, 403, status)
		status, _ = call("PUT", base, adminKey, configuration(), "")
		require.Equal(t, 428, status)
		status, _ = call("PUT", base, adminKey, configuration(), "0")
		require.Equal(t, 409, status)
		status, _ = call("GET", "/api/v1/projects", sdkKey, nil, "")
		require.Equal(t, 401, status)
	})
	client, err := sdk.NewClient(sdk.Config{SDKKey: sdkKey, Endpoint: serverB.URL, WaitForInitial: true, PollInterval: time.Hour})
	require.NoError(t, err)
	defer client.Close()
	require.Eventually(t, func() bool { return testutilGauge(metricsB) > 0 }, 5*time.Second, 10*time.Millisecond)
	// Wait until both independent Redis subscriptions have acknowledged readiness.
	require.Eventually(t, func() bool {
		n, err := bus.Client.PubSubNumSub(ctx, "flagforge:changes").Result()
		return err == nil && n["flagforge:changes"] >= 2
	}, 5*time.Second, 10*time.Millisecond)
	t.Run("A update reaches B SDK through Redis", func(t *testing.T) {
		cfg := configuration()
		cfg.Enabled = true
		status, data := call("PUT", base, adminKey, cfg, "2")
		require.Equal(t, 200, status, string(data))
		require.Eventually(t, func() bool { return client.Status().Version == 3 }, 5*time.Second, 10*time.Millisecond)
		require.True(t, client.BoolVariation(ctx, "new-checkout", sdk.Subject{Key: "user"}, false))
	})
	t.Run("segments targeting validation and rollback", func(t *testing.T) {
		seg := types.Segment{Key: "premium", Name: "Premium", Rules: []types.Rule{{Conditions: []types.Condition{{Attribute: "plan", Operator: "equals", Values: []any{"premium"}}}}}}
		v, err := r.PutSegment(ctx, p, project, "production", 3, seg)
		require.NoError(t, err)
		require.Equal(t, int64(4), v)
		cfg := configuration()
		cfg.Enabled = true
		cfg.DefaultVariation = "off"
		cfg.Rules = []types.Rule{{ID: "premium", Variation: "on", Segments: []string{"premium"}}}
		f, err = r.UpdateConfig(ctx, p, project, "production", "new-checkout", 3, cfg)
		require.NoError(t, err)
		require.Equal(t, int64(5), f.Version)
		require.Eventually(t, func() bool { return client.Status().Version == 5 }, 5*time.Second, 10*time.Millisecond)
		require.True(t, client.BoolVariation(ctx, "new-checkout", sdk.Subject{Key: "user", Attributes: map[string]any{"plan": "premium"}}, false))
		require.False(t, client.BoolVariation(ctx, "new-checkout", sdk.Subject{Key: "user"}, true))
		status, _ := call("POST", base+"/rollback/3", adminKey, nil, "5")
		require.Equal(t, 200, status)
	})
	t.Run("multi-worker scheduler exactly once", func(t *testing.T) {
		cfg := configuration()
		scheduled, err := r.Schedule(ctx, p, project, "production", "new-checkout", 6, time.Now().Add(100*time.Millisecond), cfg)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			var status string
			_ = r.Pool.QueryRow(ctx, `SELECT status FROM scheduled_changes WHERE id=$1`, scheduled.ID).Scan(&status)
			return status == "APPLIED"
		}, 5*time.Second, 10*time.Millisecond)
		current, err := r.Config(ctx, project, "production", "new-checkout")
		require.NoError(t, err)
		require.Equal(t, int64(7), current.Version)
		var count int
		require.NoError(t, r.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='SCHEDULE_APPLIED' AND after_state->>'id'=$1`, scheduled.ID).Scan(&count))
		require.Equal(t, 1, count)
	})
	t.Run("schedule conflict preserves manual change", func(t *testing.T) {
		scheduled, err := r.Schedule(ctx, p, project, "production", "new-checkout", 7, time.Now().Add(300*time.Millisecond), configuration())
		require.NoError(t, err)
		cfg := configuration()
		cfg.Enabled = true
		_, err = r.UpdateConfig(ctx, p, project, "production", "new-checkout", 7, cfg)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			var status string
			_ = r.Pool.QueryRow(ctx, `SELECT status FROM scheduled_changes WHERE id=$1`, scheduled.ID).Scan(&status)
			return status == "CONFLICT"
		}, 5*time.Second, 10*time.Millisecond)
	})
	t.Run("gradual stages follow actual committed revisions", func(t *testing.T) {
		cfg := configuration()
		cfg.Enabled = true
		stages, err := r.ScheduleStages(ctx, p, project, "production", "new-checkout", 8, []repository.ScheduledChange{{Config: cfg, RunAt: time.Now().Add(100 * time.Millisecond)}, {Config: configuration(), RunAt: time.Now().Add(300 * time.Millisecond)}})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			var status string
			_ = r.Pool.QueryRow(ctx, `SELECT status FROM scheduled_changes WHERE id=$1`, stages[1].ID).Scan(&status)
			return status == "APPLIED"
		}, 5*time.Second, 10*time.Millisecond)
		current, err := r.Config(ctx, project, "production", "new-checkout")
		require.NoError(t, err)
		require.Equal(t, int64(10), current.Version)
		require.False(t, current.Enabled)
	})
	t.Run("audit immutable and complete", func(t *testing.T) {
		logs, err := r.Audit(ctx, map[string]string{"project": project}, 0)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(logs), 10)
		_, err = r.Pool.Exec(ctx, `UPDATE audit_logs SET actor='tampered' WHERE project=$1`, project)
		require.Error(t, err)
	})
	t.Run("ETag and analytics isolation", func(t *testing.T) {
		req, err := http.NewRequest("GET", serverA.URL+"/sdk/v1/config", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+sdkKey)
		req.Header.Set("If-None-Match", `"10"`)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, 304, resp.StatusCode)
		status, _ := call("POST", "/sdk/v1/events/batch", sdkKey, map[string]any{"events": []types.EvaluationEvent{{Flag: "new-checkout", Variation: "off", Reason: "FLAG_DISABLED"}}}, "")
		require.Equal(t, 202, status)
		usage, err := r.Usage(ctx, project, false)
		require.NoError(t, err)
		require.NotEmpty(t, usage)
		_, err = r.Usage(ctx, project, true)
		require.NoError(t, err)
	})
	t.Run("key rotation and revocation", func(t *testing.T) {
		id, key, err := r.RotateSDKKey(ctx, p, project, "production")
		require.NoError(t, err)
		_, err = r.Authenticate(ctx, key, true)
		require.NoError(t, err)
		require.NoError(t, r.RevokeSDKKey(ctx, p, project, "production", id))
		_, err = r.Authenticate(ctx, key, true)
		require.ErrorIs(t, err, repository.ErrNotFound)
		_, err = r.Authenticate(ctx, sdkKey, true)
		require.NoError(t, err)
	})
	t.Run("kill switch reaches local SDK", func(t *testing.T) {
		cfg := configuration()
		cfg.Enabled = true
		_, err = r.UpdateConfig(ctx, p, project, "production", "new-checkout", 10, cfg)
		require.NoError(t, err)
		require.Eventually(t, func() bool { return client.Status().Version == 11 }, 5*time.Second, 10*time.Millisecond)
		status, _ := call("POST", base+"/disable", adminKey, nil, "11")
		require.Equal(t, 200, status)
		require.Eventually(t, func() bool { return client.Status().Version == 12 }, 5*time.Second, 10*time.Millisecond)
		require.False(t, client.BoolVariation(ctx, "new-checkout", sdk.Subject{Key: "user"}, true))
	})
	// Stream shutdown must precede httptest.Close, otherwise Close waits on SSE.
	_ = client.Close()
	cancel()
	joinA()
	joinB()
}

func testutilGauge(m *observability.Metrics) float64 {
	metric := &dto.Metric{}
	_ = m.Connections.Write(metric)
	return metric.GetGauge().GetValue()
}
