package xredis

import (
	"math"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type configCase struct {
	name       string
	change     func(*Config)
	requires64 bool
}

func testPointer[T any](value T) *T { return &value }

func TestConfigValidate(t *testing.T) {
	cases := []configCase{
		{name: "empty alias", change: func(c *Config) { c.Alias = "" }},
		{name: "alias whitespace", change: func(c *Config) { c.Alias = " redis " }},
		{name: "empty address", change: func(c *Config) { c.Addr = "" }},
		{name: "negative database", change: func(c *Config) { c.DB = -1 }},
		{name: "zero startup timeout", change: func(c *Config) { c.PingTimeout = testPointer(uint(0)) }},
		{name: "duration overflow", change: func(c *Config) { c.ReadTimeout = testPointer(^uint(0)) }, requires64: true},
		{name: "negative pool size", change: func(c *Config) { c.PoolSize = testPointer(-1) }},
		{name: "negative idle connections", change: func(c *Config) { c.MinIdleConn = testPointer(-1) }},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if item.requires64 && strconv.IntSize < 64 {
				t.Skip("duration overflow requires a 64-bit uint")
			}
			config := Config{Alias: "test", Addr: "localhost:6379"}
			item.change(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	var absent *Config
	if absent.Validate() == nil || absent.FillDefaults() != nil {
		t.Fatal("nil configuration contract changed")
	}
}

func TestConfigNativeOverridesAndImmutability(t *testing.T) {
	config := Config{
		Alias: "test", Addr: "fallback:6379", DB: 4,
		ReadTimeout: testPointer(^uint(0)), PoolSize: testPointer(-1),
		ExtraOptions: &redis.Options{Addr: "native:6379", ReadTimeout: time.Second, PoolSize: 2},
	}
	before := *config.ExtraOptions
	options, err := config.options()
	if err != nil {
		t.Fatal(err)
	}
	if options.Addr != "native:6379" || options.ReadTimeout != time.Second || options.PoolSize != 2 {
		t.Fatal("native override lost")
	}
	if options.DB != 4 || !options.ContextTimeoutEnabled {
		t.Fatal("database fallback or context timeout policy changed")
	}
	if config.PingTimeout != nil || !reflect.DeepEqual(before, *config.ExtraOptions) {
		t.Fatal("validation mutated caller-owned configuration")
	}
	config.DB = 0
	options, err = config.options()
	if err != nil || options.DB != 0 {
		t.Fatalf("explicit database zero rejected: %v", err)
	}
	config.FillDefaults()
	if config.PingTimeout == nil || *config.PingTimeout != 10 {
		t.Fatal("startup timeout default changed")
	}
}

func TestConfigPoolRange(t *testing.T) {
	config := Config{
		Alias: "test", Addr: "localhost:6379",
		ExtraOptions: &redis.Options{
			PoolSize: math.MaxInt32, MinIdleConns: math.MaxInt32,
			MaxIdleConns: math.MaxInt32, MaxActiveConns: math.MaxInt32,
		},
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}
