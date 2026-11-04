//go:build integration

package xredis_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// integrationClient never guesses a local Redis address or removes unrelated keys.
func integrationClient(t *testing.T) (*redis.Client, string) {
	t.Helper()
	address := os.Getenv("XREDIS_REDIS_URL")
	if address == "" {
		t.Skip("set XREDIS_REDIS_URL to an isolated Redis URL")
	}
	options, err := redis.ParseURL(address)
	if err != nil {
		t.Fatal(err)
	}
	options.ContextTimeoutEnabled = true
	client := redis.NewClient(options)
	prefix := "xredis:test:{" + uuid.NewString() + "}"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, prefix+"*", 100).Result()
			if err != nil {
				t.Error(err)
				break
			}
			if len(keys) > 0 {
				if err := client.Del(ctx, keys...).Err(); err != nil {
					t.Error(err)
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := client.Ping(t.Context()).Err(); err != nil {
		t.Fatal(err)
	}
	return client, prefix
}
