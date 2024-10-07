package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Server struct {
		Port       int    `yaml:"port"`
		Pprof      bool   `yaml:"pprof"`
		AdminToken string `yaml:"admin_token"`
	} `yaml:"server"`
	Database struct {
		DSN string `yaml:"dsn"`
	} `yaml:"database"`
	Redis struct {
		Address  string `yaml:"address"`
		Password string `yaml:"password"`
	} `yaml:"redis"`
	Streaming struct {
		HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	} `yaml:"streaming"`
	Outbox struct {
		BatchSize    int           `yaml:"batch_size"`
		PollInterval time.Duration `yaml:"poll_interval"`
	} `yaml:"outbox"`
	SDK struct {
		SnapshotCacheTTL time.Duration `yaml:"snapshot_cache_ttl"`
	} `yaml:"sdk"`
}

func Load(path string) (Config, error) {
	var c Config
	c.Server.Port = 8080
	c.Redis.Address = "localhost:6379"
	c.Streaming.HeartbeatInterval = 15 * time.Second
	c.Outbox.BatchSize = 100
	c.Outbox.PollInterval = 500 * time.Millisecond
	c.SDK.SnapshotCacheTTL = time.Minute
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		if err = yaml.Unmarshal(b, &c); err != nil {
			return c, err
		}
	}
	for env, dest := range map[string]*string{"FLAGFORGE_DATABASE_DSN": &c.Database.DSN, "FLAGFORGE_REDIS_ADDRESS": &c.Redis.Address, "FLAGFORGE_REDIS_PASSWORD": &c.Redis.Password, "FLAGFORGE_ADMIN_TOKEN": &c.Server.AdminToken} {
		if v, ok := os.LookupEnv(env); ok {
			*dest = v
		}
	}
	for env, dest := range map[string]*int{"FLAGFORGE_PORT": &c.Server.Port, "FLAGFORGE_OUTBOX_BATCH_SIZE": &c.Outbox.BatchSize} {
		if v, ok := os.LookupEnv(env); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return c, fmt.Errorf("invalid %s", env)
			}
			*dest = n
		}
	}
	for env, dest := range map[string]*time.Duration{"FLAGFORGE_HEARTBEAT_INTERVAL": &c.Streaming.HeartbeatInterval, "FLAGFORGE_OUTBOX_POLL_INTERVAL": &c.Outbox.PollInterval, "FLAGFORGE_SNAPSHOT_CACHE_TTL": &c.SDK.SnapshotCacheTTL} {
		if v, ok := os.LookupEnv(env); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return c, fmt.Errorf("invalid %s", env)
			}
			*dest = d
		}
	}
	if v, ok := os.LookupEnv("FLAGFORGE_PPROF"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, err
		}
		c.Server.Pprof = b
	}
	if c.Database.DSN == "" || len(c.Server.AdminToken) < 24 || c.Server.Port < 1 || c.Server.Port > 65535 || c.Outbox.BatchSize < 1 || c.Outbox.BatchSize > 1000 || c.Outbox.PollInterval <= 0 || c.Streaming.HeartbeatInterval <= 0 || c.SDK.SnapshotCacheTTL <= 0 {
		return c, fmt.Errorf("invalid config: database DSN, admin token (24+ chars), port and positive intervals required")
	}
	return c, nil
}
