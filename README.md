# xredis

`xredis` is a lightweight utility library built on top of `go-redis/v9`, with three core capabilities:

- Redis client initialization and lifecycle management for multiple instances (`Init/Get/Close/CloseAll`)
- Lua-based distributed locks (single-key and multi-key atomic lock)
- Lua-based rate limiting (single-rule and multi-rule atomic checks, suitable for captcha throttling)

## Installation

```bash
go get github.com/lazyboon/xredis
```

## 1. Initialize and Retrieve Redis Clients

```go
package main

import (
	"log"

	"github.com/lazyboon/xredis"
)

func ptrU(v uint) *uint { return &v }

func main() {
	err := xredis.Init([]*xredis.Config{
		{
			Alias:       "cache",
			Addr:        "127.0.0.1:6379",
			DB:          0,
			PingTimeout: ptrU(3),
		},
		{
			Alias: "queue",
			Addr:  "127.0.0.1:6380",
			DB:    1,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer xredis.CloseAll()

	cache := xredis.Get("cache")
	if cache == nil {
		log.Fatal("cache client not found")
	}

	_ = cache
}
```

### Config Priority

- Fields in `Config.ExtraOptions` have the highest priority.
- Missing fields in `ExtraOptions` fall back to `Config` fields.
- `PingTimeout` defaults to 10 seconds via `FillDefaults()`.

## 2. Distributed Lock

### Single-key Lock

```go
package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/lazyboon/xredis"
)

func main() {
	ctx := context.Background()
	rdb := xredis.Get("cache")
	if rdb == nil {
		log.Fatal("redis client not found")
	}

	lockMgr := xredis.NewLockManager(rdb)
	lock, err := lockMgr.Obtain(ctx, "lock:order:123", 10*time.Second)
	if err != nil {
		if errors.Is(err, xredis.ErrNotObtained) {
			log.Println("lock is contended")
			return
		}
		log.Fatal(err)
	}
	defer func() {
		_ = lock.Release(ctx)
	}()

	ttl, err := lock.TTL(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("lock ttl=%s token=%s", ttl, lock.Token())

	if err = lock.Refresh(ctx, 10*time.Second); err != nil {
		log.Fatal(err)
	}
}
```

### Multi-key Atomic Lock

```go
keys := []string{
	"lock:wallet:user:1001",
	"lock:inventory:sku:888",
}

lock, err := lockMgr.ObtainMulti(ctx, keys, 8*time.Second)
if err != nil {
	// If any key is contended, the whole acquire fails (all-or-nothing).
	return
}
defer lock.Release(ctx)
```

## 3. Rate Limiting

### 3.1 Single-rule Rate Limit

```go
limiter := xredis.NewLimiter(rdb)

// 5 requests per minute, burst defaults to rate (5).
limit := xredis.NewPerMinuteLimit(5)
res, err := limiter.Allow(ctx, "api:send_code:user:1001", limit)
if err != nil {
	return err
}
if res.Allowed == 0 {
	// Rate-limited, wait for res.RetryAfter.
	return nil
}
```

### 3.2 Multi-rule Atomic Rate Limit (Recommended for Captcha)

All rules are checked in one script call, and state is updated only if all rules pass.

```go
rules := []xredis.Rule{
	// 1 request per minute
	{Key: "captcha:send:user:1001:1m", Limit: xredis.NewPerMinuteLimit(1)},
	// 5 requests per hour
	{Key: "captcha:send:user:1001:1h", Limit: xredis.NewPerHourLimit(5)},
	// 20 requests per day
	{Key: "captcha:send:user:1001:1d", Limit: xredis.NewPerDayLimit(20)},
}

res, err := limiter.AllowNRules(ctx, rules, 1)
if err != nil {
	return err
}
if res.Allowed == 0 {
	// Which rule blocked this request:
	// res.LimitedBy.Index: index in rules
	// res.LimitedBy.Key:   key of that rule
	// res.RetryAfter:      suggested wait duration before retry
	return nil
}
```

### AllowNRules vs AllowAtMostRules

- `AllowNRules(ctx, rules, n)`: allows exactly `n` or `0`.
- `AllowAtMostRules(ctx, rules, n)`: allows a value in `[0, n]` (best-effort admission).

## 4. Common Errors

- `xredis.ErrNotObtained`: lock acquisition failed due to contention.
- `xredis.ErrLockNotHeld`: lock refresh/release called when lock is no longer owned.
- `xredis.ErrInvalidInput`: invalid limiter input (empty key, empty rules, `n <= 0`, etc).
- `xredis.ErrScriptProtocol`: unexpected Lua result protocol.

## 5. Release Notes for This Repository

Before tagging a release, verify:

1. Go version strategy: current `go.mod` is `go 1.25`; make sure local and CI environments can install/use it.
2. Test coverage: add integration tests for lock and limiter critical paths.
3. Changelog: add a `CHANGELOG.md` with API and behavior notes.
4. Versioning: use clear semver tags (`v0.x` for fast iteration or `v1.0.0` for stable API).

