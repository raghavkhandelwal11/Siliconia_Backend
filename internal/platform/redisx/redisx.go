// Package redisx opens the Redis client and provides single-runner job locks (LLD 6, 11).
package redisx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Open connects to Redis and checks the connection.
func Open(ctx context.Context, url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis: parse url: %w", err)
	}
	c := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis: ping: %w", err)
	}
	return c, nil
}

// ErrLocked means another runner holds the lock.
var ErrLocked = errors.New("redisx: lock held by another runner")

// releaseScript deletes the lock only if we still own it.
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

// Lock is a held `lock:<name>` key (SET NX PX).
type Lock struct {
	client *redis.Client
	key    string
	token  string
}

// TryLock takes `lock:<name>` for ttl, or returns ErrLocked.
func TryLock(ctx context.Context, c *redis.Client, name, token string, ttl time.Duration) (*Lock, error) {
	key := "lock:" + name
	ok, err := c.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("redisx: lock %s: %w", name, err)
	}
	if !ok {
		return nil, ErrLocked
	}
	return &Lock{client: c, key: key, token: token}, nil
}

// Release frees the lock if it is still ours.
func (l *Lock) Release(ctx context.Context) error {
	return releaseScript.Run(ctx, l.client, []string{l.key}, l.token).Err()
}
