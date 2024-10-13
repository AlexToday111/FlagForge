// Command loadtest measures local evaluation or control-plane/SDK propagation.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/flagforge/flagforge/pkg/sdk"
	"github.com/flagforge/flagforge/pkg/types"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	i := int(float64(len(samples)-1) * p)
	return samples[i]
}
func report(name string, samples []time.Duration) {
	fmt.Printf("%s samples=%d p50=%s p95=%s p99=%s\n", name, len(samples), percentile(samples, .5), percentile(samples, .95), percentile(samples, .99))
}
func run() error {
	mode := flag.String("mode", "local", "local or propagation")
	n := flag.Int("n", 1000000, "local evaluation count")
	clients := flag.Int("clients", 10, "SDK clients")
	updates := flag.Int("updates", 10, "configuration updates")
	endpoint := flag.String("endpoint", "http://localhost:18080", "control plane URL")
	sdkEndpoint := flag.String("sdk-endpoint", "http://localhost:18080", "SDK gateway URL")
	sdkKey := flag.String("sdk-key", os.Getenv("FLAGFORGE_SDK_KEY"), "SDK key")
	admin := flag.String("token", os.Getenv("FLAGFORGE_ADMIN_TOKEN"), "admin token")
	project := flag.String("project", "demo", "project")
	env := flag.String("env", "production", "environment")
	key := flag.String("flag", "new-checkout", "flag key")
	flag.Parse()
	ctx := context.Background()
	if *mode == "local" {
		if *n < 1 {
			return fmt.Errorf("n must be positive")
		}
		f := types.FlagConfig{Key: "checkout", Type: types.Boolean, Enabled: true, Variations: map[string]any{"on": true, "off": false}, DefaultVariation: "off", OffVariation: "off", Rollout: []types.RolloutVariation{{VariationKey: "on", Weight: 20000}, {VariationKey: "off", Weight: 80000}}, Version: 1}
		s := types.Snapshot{Project: "benchmark", Environment: "production", Version: 1, Flags: map[string]types.FlagConfig{f.Key: f}}
		client, err := sdk.NewClient(sdk.Config{Offline: true, Bootstrap: &s})
		if err != nil {
			return err
		}
		defer client.Close()
		subject := sdk.Subject{Key: "benchmark-user"}
		stride := *n / 100000
		if stride < 1 {
			stride = 1
		}
		samples := make([]time.Duration, 0, 100000)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		started := time.Now()
		on := 0
		for i := 0; i < *n; i++ {
			if i%stride == 0 && len(samples) < 100000 {
				start := time.Now()
				if client.BoolVariation(ctx, "checkout", subject, false) {
					on++
				}
				samples = append(samples, time.Since(start))
			} else {
				if client.BoolVariation(ctx, "checkout", subject, false) {
					on++
				}
			}
		}
		elapsed := time.Since(started)
		runtime.ReadMemStats(&after)
		fmt.Printf("evaluations=%d enabled=%d elapsed=%s ops/sec=%.0f ns/op=%.1f allocations/op=%.4f bytes/op=%.2f\n", *n, on, elapsed, float64(*n)/elapsed.Seconds(), float64(elapsed.Nanoseconds())/float64(*n), float64(after.Mallocs-before.Mallocs)/float64(*n), float64(after.TotalAlloc-before.TotalAlloc)/float64(*n))
		report("sampled local latency (includes timer overhead)", samples)
		return nil
	}
	if *mode != "propagation" || *clients < 1 || *clients > 10000 || *updates < 1 || *updates > 10000 || *sdkKey == "" || *admin == "" {
		return fmt.Errorf("valid mode, credentials, client/update counts required")
	}
	pool := make([]*sdk.Client, 0, *clients)
	defer func() {
		for _, c := range pool {
			_ = c.Close()
		}
	}()
	for i := 0; i < *clients; i++ {
		c, err := sdk.NewClient(sdk.Config{Endpoint: *sdkEndpoint, SDKKey: *sdkKey, WaitForInitial: true, PollInterval: time.Minute})
		if err != nil {
			return err
		}
		pool = append(pool, c)
	}
	httpClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	path := *endpoint + "/api/v1/projects/" + *project + "/environments/" + *env + "/flags/" + *key
	call := func(method string, f *types.FlagConfig) (types.FlagConfig, error) {
		var body io.Reader
		if f != nil {
			b, _ := json.Marshal(f)
			body = bytes.NewReader(b)
		}
		r, err := http.NewRequest(method, path, body)
		if err != nil {
			return types.FlagConfig{}, err
		}
		r.Header.Set("Authorization", "Bearer "+*admin)
		if f != nil {
			r.Header.Set("If-Match", strconv.FormatInt(f.Version, 10))
			r.Header.Set("Content-Type", "application/json")
		}
		resp, err := httpClient.Do(r)
		if err != nil {
			return types.FlagConfig{}, fmt.Errorf("admin request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return types.FlagConfig{}, fmt.Errorf("admin status %d", resp.StatusCode)
		}
		var out types.FlagConfig
		err = json.NewDecoder(resp.Body).Decode(&out)
		return out, err
	}
	f, err := call("GET", nil)
	if err != nil {
		return err
	}
	original := f
	defer func() {
		latest, err := call("GET", nil)
		if err == nil {
			original.Version = latest.Version
			_, err = call("PUT", &original)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "restore original configuration failed")
		}
	}()
	samples := make([]time.Duration, 0, *clients**updates)
	apiSamples := make([]time.Duration, 0, *updates)
	started := time.Now()
	for u := 0; u < *updates; u++ {
		f.Enabled = !f.Enabled
		t0 := time.Now()
		f, err = call("PUT", &f)
		if err != nil {
			return err
		}
		apiSamples = append(apiSamples, time.Since(t0))
		seen := make([]bool, len(pool))
		remaining := len(pool)
		for remaining > 0 {
			for i, c := range pool {
				if !seen[i] && c.Status().Version >= f.Version {
					seen[i] = true
					remaining--
					samples = append(samples, time.Since(t0))
				}
			}
			if time.Since(t0) > 15*time.Second {
				return fmt.Errorf("propagation timeout: %d clients remain", remaining)
			}
			time.Sleep(time.Millisecond)
		}
	}
	fmt.Printf("clients=%d updates=%d updates/sec=%.2f\n", *clients, *updates, float64(*updates)/time.Since(started).Seconds())
	report("admin API latency", apiSamples)
	report("update request to SDK observation", samples)
	return nil
}
