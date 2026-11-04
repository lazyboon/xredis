package xredis_test

import (
	"fmt"
	"github.com/lazyboon/xredis"
	"github.com/redis/go-redis/v9"
	"math"
	"testing"
)

func TestEffectivePoolCounts(t *testing.T) {
	for _, field := range []string{"PoolSize", "MinIdleConns", "MaxIdleConns", "MaxActiveConns"} {
		for _, value := range []int{-1, overflowingPoolCount(t)} {
			t.Run(fmt.Sprintf("%s/%d", field, value), func(t *testing.T) {
				opts := &redis.Options{}
				switch field {
				case "PoolSize":
					opts.PoolSize = value
				case "MinIdleConns":
					opts.MinIdleConns = value
				case "MaxIdleConns":
					opts.MaxIdleConns = value
				case "MaxActiveConns":
					opts.MaxActiveConns = value
				}
				cfg := &xredis.Config{Alias: "invalid", Addr: "audit.invalid:6379", ExtraOptions: opts}
				if cfg.Validate() == nil {
					t.Fatal("Validate accepted invalid pool count")
				}
				if xredis.Init([]*xredis.Config{cfg}) == nil {
					t.Fatal("Init accepted invalid pool count")
				}
			})
		}
	}
	huge := overflowingPoolCount(t)
	for _, cfg := range []*xredis.Config{
		{Alias: "base-pool", Addr: "audit.invalid:6379", PoolSize: &huge},
		{Alias: "base-idle", Addr: "audit.invalid:6379", MinIdleConn: &huge},
	} {
		if cfg.Validate() == nil {
			t.Fatalf("%s accepted overflowing Config field", cfg.Alias)
		}
	}
	cfg := &xredis.Config{Alias: "override", Addr: "audit.invalid:6379", PoolSize: &huge, MinIdleConn: &huge, ExtraOptions: &redis.Options{PoolSize: 1, MinIdleConns: 1}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid native override rejected: %v", err)
	}
	cfg.ExtraOptions = &redis.Options{PoolSize: math.MaxInt32, MinIdleConns: math.MaxInt32, MaxIdleConns: math.MaxInt32, MaxActiveConns: math.MaxInt32}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("int32 boundary rejected: %v", err)
	}
}
