package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/flagforge/flagforge/pkg/types"
	"github.com/redis/go-redis/v9"
)

const topic = "flagforge:changes"

type Redis struct{ Client *redis.Client }

func New(address, password string) *Redis {
	return &Redis{Client: redis.NewClient(&redis.Options{Addr: address, Password: password, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})}
}
func (r *Redis) Publish(ctx context.Context, e types.Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return r.Client.Publish(ctx, topic, b).Err()
}
func (r *Redis) Subscribe(ctx context.Context, deliver func(types.Event)) error {
	sub := r.Client.Subscribe(ctx, topic)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		return err
	}
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("redis subscription closed")
			}
			var e types.Event
			if json.Unmarshal([]byte(msg.Payload), &e) == nil {
				deliver(e)
			}
		}
	}
}

// GetSnapshot uses a version-qualified key: stale entries can never satisfy a
// request for a newer database revision. Redis failure falls back to PostgreSQL.
func (r *Redis) GetSnapshot(ctx context.Context, envID string, version int64) (types.Snapshot, bool) {
	b, err := r.Client.Get(ctx, fmt.Sprintf("flagforge:snapshot:%s:%d", envID, version)).Bytes()
	if err != nil {
		return types.Snapshot{}, false
	}
	var s types.Snapshot
	if json.Unmarshal(b, &s) != nil || s.Version != version {
		return s, false
	}
	return s, true
}
func (r *Redis) CacheSnapshot(ctx context.Context, envID string, s types.Snapshot, ttl time.Duration) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return r.Client.Set(ctx, fmt.Sprintf("flagforge:snapshot:%s:%d", envID, s.Version), b, ttl).Err()
}
