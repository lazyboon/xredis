//go:build integration

package xredis_test

import (
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/lazyboon/xredis"
	"github.com/redis/go-redis/v9"
)

func TestNamedClientWithNativeURL(t *testing.T) {
	address := os.Getenv("XREDIS_REDIS_URL")
	if address == "" {
		t.Skip("set XREDIS_REDIS_URL to an isolated Redis URL")
	}
	options, err := redis.ParseURL(address)
	if err != nil {
		t.Fatal(err)
	}
	alias := "test-" + uuid.NewString()
	if err := xredis.InitContext(t.Context(), []*xredis.Config{{Alias: alias, ExtraOptions: options}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := xredis.Close(alias); err != nil {
			t.Error(err)
		}
	})
	client := xredis.Get(alias)
	if client == nil {
		t.Fatal("client was not registered")
	}
	if err := client.Ping(t.Context()).Err(); err != nil {
		t.Fatal(err)
	}
	if err := xredis.Close(alias); err != nil {
		t.Fatal(err)
	}
	if xredis.Get(alias) != nil {
		t.Fatal("closed client remained registered")
	}
}
