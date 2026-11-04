# xredis

Named [go-redis](https://github.com/redis/go-redis) clients, atomic Redis leases,
and multi-rule rate limiting. Locks and limiters also accept caller-owned clients;
the named-client registry is optional.

Requires **Go 1.27.3+**. Integration tests cover Redis 6.2, 7.4, and 8.

```sh
go get github.com/lazyboon/xredis
```

## Clients

Initialize clients during application startup, then reuse them. Configuration
durations are in **seconds**. `ExtraOptions` accepts native TLS, credentials,
connection hooks, and other go-redis settings.

```go
options, err := redis.ParseURL(os.Getenv("REDIS_URL"))
if err != nil {
    return err
}
if err := xredis.InitContext(ctx, []*xredis.Config{
    {Alias: "main", ExtraOptions: options},
}); err != nil {
    return err
}
client := xredis.Get("main")
```

Use imports `github.com/lazyboon/xredis` and
`github.com/redis/go-redis/v9`. Stop consumers before `Close("main")` or
`CloseAll()`, and handle their returned errors. The registry owns registered
clients; consumers must not close the shared client directly.

- Initialization prepares the complete batch before registration; failure closes
  the clients prepared by that call and leaves the registry unchanged.
- `InitContext` bounds context-aware startup work. Custom dialers and connection
  hooks must honor their context; the library cannot interrupt arbitrary callbacks.
  Canceling that context after success does not
  shut down the running clients. Do not overlap startup with registry shutdown.
- Unset pointer fields preserve native defaults. `PingTimeout` defaults to ten
  seconds and cannot be zero. Native nonzero overlapping values take precedence.
- `ExtraOptions.DB == 0` falls back to `Config.DB`. To select Redis database zero,
  set `Config.DB` to zero as well.
- Native `ReadTimeout`/`WriteTimeout == -2` retain their normal no-deadline
  behavior. In that case, startup uses and closes a separate probe pool, and the
  runtime client connects on demand. Native connection hooks run for every
  physical connection, including probe connections.

## Leases

```go
locks := xredis.NewLockManager(client)
lease, err := locks.Obtain(ctx, "jobs:{123}:lock", 30*time.Second)
if errors.Is(err, xredis.ErrNotObtained) {
    // Another owner holds the lease.
    return err
}
if err != nil {
    return err
}
// Finish protected work before expiry, or explicitly renew with lease.Refresh.
return lease.Release(ctx)
```

Use `ObtainMulti` for all-or-nothing acquisition of multiple keys. Keys must be
unique and share one hash slot in Redis Cluster. TTLs must be at least one
millisecond; fractional milliseconds are truncated.

Ownership is checked by every operation. Tokens may contain colons and binary
bytes. Omitted tokens are generated independently; use different tokens for
independent executions. An explicit same-token retry can renew the lease and
replace its metadata. After changing metadata, operate through the newly returned
`Lock`; the old handle no longer matches.

**Leases do not renew automatically.** Context cancellation does not release an
acquired lease or stop work that has already started. Lease expiry alone cannot
prevent an expired holder from continuing to modify an external resource.

By default acquisition makes one attempt. `WithLockRetryStrategy` enables
contention retries. Supply a caller deadline to bound waiting; without one,
explicit retries have an acquisition window equal to the requested lease TTL.
Non-contention Redis failures are not retried by this layer. An absent lease
returns `ErrLockNotHeld` from `Release`/`Refresh`, and a zero TTL from `TTL`.

## Rate limits

```go
limiter := xredis.NewLimiter(client)
result, err := limiter.Allow(ctx, "api:{123}:minute", xredis.NewPerMinuteLimit(120, 10))
if err != nil {
    return err
}
if result.Allowed == 0 {
    // Denial is a normal result. RetryAfter is the delay before another attempt.
}
```

`AllowN` admits the complete cost or none. `AllowAtMost` admits a partial cost.
`AllowNRules` and `AllowAtMostRules` apply multiple rules atomically; a denied
full-cost request consumes no quota from any rule. `LimitedBy` identifies the
constraining rule. `Reset` deletes the selected quota state.

Rules using the same key share state. Reset the key, or choose a new one, when
changing a rule. The package does not add key prefixes. Multi-key operations
require a shared Cluster hash slot. A period is truncated to microseconds, and
the replenishment interval must be at least one microsecond. Invalid parameters
return `ErrInvalidInput`; malformed script replies return `ErrScriptProtocol`.
Quota state expires automatically using Redis server time.

The package does not own caller-provided clients. Timeouts, authentication, TLS,
primary routing, and client-level retry settings remain the caller's responsibility.
Lua isolates operations; a transport failure can leave a write's outcome unknown.

## Testing

```sh
go test -race -shuffle=on -count=1 ./...
go vet ./...
```

Default tests use local in-memory protocol fixtures and do not require Redis.
The integration suite executes the real Lua scripts. It only uses randomly
namespaced keys and never flushes a database:

```sh
XREDIS_REDIS_URL=redis://127.0.0.1:6379/0 \
  go test -race -shuffle=on -count=1 -tags integration ./...
```

Use an isolated test server. URLs support authentication and a database number.
Without `XREDIS_REDIS_URL`, integration tests skip rather than guessing an address.
CI runs them against Redis 6.2, 7.4, and 8.

## Migrating from earlier v1 releases

This update stays on the v1 module path while changing several APIs. Review the
pointer-valued optional configuration fields and error-returning shutdown methods.
Custom lock and limiter client interfaces use the `I` prefix.

Lock values now use a byte-length-prefixed owner format. Allow old leases to
expire, release them before upgrading, or use a new key namespace; do not mix
old and new lease owners on the same keys. Same-token retries remain intentional.
Rate-limiter precision and duration validation reject previously accepted unsafe
inputs. Native options and startup timeout behavior are described above.

## License

[MIT](LICENSE).
