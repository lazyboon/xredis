package xredis

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lazyboon/xretry"
	"github.com/redis/go-redis/v9"
)

// IRedisClient is the script execution contract required by distributed locks.
type IRedisClient interface {
	redis.Scripter
}

var (
	// ErrNotObtained indicates that another token holds at least one requested key.
	ErrNotObtained = errors.New("xredis: lock not obtained")

	// ErrLockNotHeld indicates that the lock is absent or no longer matches the caller's value.
	ErrLockNotHeld = errors.New("xredis: lock not held")

	// Internal errors distinguish invalid arguments from unavailable dependencies.
	errEmptyLockKeys    = errors.New("xredis: lock keys cannot be empty")
	errEmptyLockKey     = errors.New("xredis: lock key cannot be empty")
	errDuplicateLockKey = errors.New("xredis: lock keys must be unique")
	errInvalidTTL       = errors.New("xredis: lock ttl must be at least one millisecond")
	errNilLockManager   = errors.New("xredis: lock manager is nil")
	errNilRedisClient   = errors.New("xredis: redis client is nil")
)

//go:embed lua/lock_obtain.lua
var obtainLua string

//go:embed lua/lock_refresh.lua
var refreshLua string

//go:embed lua/lock_release.lua
var releaseLua string

//go:embed lua/lock_pttl.lua
var pttlLua string

var (
	obtainScript  = redis.NewScript(obtainLua)
	refreshScript = redis.NewScript(refreshLua)
	releaseScript = redis.NewScript(releaseLua)
	pttlScript    = redis.NewScript(pttlLua)
)

// LockOption configures an acquisition attempt.
type LockOption func(*LockOptions)

// LockOptions defines token, metadata, and retry settings for acquisition.
type LockOptions struct {
	// Token identifies the owner. Nil or empty values generate a UUID.
	// Independent executions must use different tokens.
	Token *string
	// Metadata follows the ownership prefix and is ignored when retrying with the same token.
	Metadata *string
	// RetryStrategy controls retries on contention. Nil disables application-level retries.
	RetryStrategy xretry.Strategy
}

// NewLockOption returns an empty option container with default acquisition behavior.
func NewLockOption() *LockOptions {
	return &LockOptions{}
}

// Apply applies non-nil options in order and returns the receiver.
func (o *LockOptions) Apply(options ...LockOption) *LockOptions {
	for _, option := range options {
		if option != nil {
			option(o)
		}
	}
	return o
}

// WithLockToken sets the ownership token; an empty token requests an automatically generated UUID.
func WithLockToken(token string) LockOption {
	return func(opts *LockOptions) {
		opts.Token = &token
	}
}

// WithLockMetadata attaches metadata to the stored lock value.
// After a same-token retry changes metadata, refresh and release through the newly returned Lock.
func WithLockMetadata(metadata string) LockOption {
	return func(opts *LockOptions) {
		opts.Metadata = &metadata
	}
}

// WithLockRetryStrategy sets the retry policy for contention.
// Without a caller deadline, an explicitly configured policy receives an acquisition window of one TTL.
func WithLockRetryStrategy(strategy xretry.Strategy) LockOption {
	return func(opts *LockOptions) {
		opts.RetryStrategy = strategy
	}
}

// LockManager manages expiring Redis leases using atomic scripts.
// It can be shared if its client supports concurrent use. The caller retains client ownership.
// Stored values use a length-prefixed format; release or expire legacy locks before mixing versions.
type LockManager struct {
	client IRedisClient
}

// NewLockManager creates a manager without acquiring a lock or opening a connection.
func NewLockManager(c IRedisClient) *LockManager {
	return &LockManager{client: c}
}

// Obtain acquires a single-key lease or returns [ErrNotObtained] on contention.
func (m *LockManager) Obtain(ctx context.Context, key string, ttl time.Duration, options ...LockOption) (*Lock, error) {
	return m.ObtainMulti(ctx, []string{key}, ttl, options...)
}

// ObtainMulti acquires all requested keys atomically or returns [ErrNotObtained] on contention.
// A retry with the same token may replace metadata and renew all keys.
// Keys must be nonempty and unique; in Redis Cluster they must share a hash slot.
// ttl must be at least one millisecond and is rounded down to whole milliseconds.
// Leases are not renewed automatically, and cancellation does not release an acquired lease.
func (m *LockManager) ObtainMulti(ctx context.Context, keys []string, ttl time.Duration, options ...LockOption) (*Lock, error) {
	if m == nil {
		return nil, errNilLockManager
	}
	if nilClient(m.client) {
		return nil, errNilRedisClient
	}

	if err := m.validateObtainArgs(keys, ttl); err != nil {
		return nil, err
	}

	opts := NewLockOption().Apply(options...)
	token := m.resolveToken(opts.Token)
	// Include the token's byte length so shorter tokens cannot match a longer owner's prefix.
	// Byte offsets match Redis GETRANGE and Lua string operations, including binary tokens.
	owner := strconv.Itoa(len(token)) + ":" + token
	value := owner
	if opts.Metadata != nil {
		value += *opts.Metadata
	}

	runCtx := ctx
	cancel := func() {}
	// Bound explicit retries only; a single attempt uses the caller's context unchanged.
	if _, ok := ctx.Deadline(); !ok && opts.RetryStrategy != nil {
		runCtx, cancel = context.WithDeadline(ctx, time.Now().Add(ttl))
	}
	defer cancel()

	err := xretry.DoActionWithContext(
		runCtx,
		m.resolveRetryStrategy(opts),
		func(attemptCtx context.Context) error {
			return m.obtainOnce(attemptCtx, keys, value, len(owner), ttl)
		},
		xretry.WithShouldRetry(func(_ context.Context, _ int, err error) bool {
			return errors.Is(err, ErrNotObtained)
		}),
	)
	if err != nil {
		return nil, m.unwrapRetryError(err)
	}

	return m.newLock(keys, value, len(owner)), nil
}

// validateObtainArgs rejects empty or duplicate keys and sub-millisecond TTLs before script execution.
func (m *LockManager) validateObtainArgs(keys []string, ttl time.Duration) error {
	if len(keys) == 0 {
		return errEmptyLockKeys
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			return errEmptyLockKey
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: %q", errDuplicateLockKey, key)
		}
		seen[key] = struct{}{}
	}
	if ttl < time.Millisecond {
		return errInvalidTTL
	}
	return nil
}

// resolveToken generates an independent owner identifier when no token is supplied.
func (m *LockManager) resolveToken(token *string) string {
	if token != nil && *token != "" {
		return *token
	}
	return uuid.NewString()
}

// resolveRetryStrategy falls back to a single attempt when retries are not configured.
func (m *LockManager) resolveRetryStrategy(opts *LockOptions) xretry.Strategy {
	if opts.RetryStrategy != nil {
		return opts.RetryStrategy
	}
	return xretry.NewZero()
}

// obtainOnce maps a script ownership rejection to [ErrNotObtained].
func (m *LockManager) obtainOnce(ctx context.Context, keys []string, value string, ownerLen int, ttl time.Duration) error {
	_, err := obtainScript.Run(ctx, m.client, keys, value, ownerLen, ttl.Milliseconds()).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ErrNotObtained
		}
		return err
	}
	return nil
}

// unwrapRetryError exposes the final attempt error without requiring callers to inspect xretry types.
func (m *LockManager) unwrapRetryError(err error) error {
	var retryErr *xretry.RetryError
	if errors.As(err, &retryErr) && retryErr.Last() != nil {
		return retryErr.Last()
	}
	return err
}

// newLock snapshots the key list so caller mutations cannot change the lease's ownership scope.
func (m *LockManager) newLock(keys []string, value string, ownerLen int) *Lock {
	keysCopy := make([]string, len(keys))
	copy(keysCopy, keys)
	return &Lock{
		client:   m.client,
		keys:     keysCopy,
		value:    value,
		ownerLen: ownerLen,
	}
}

// Lock identifies an acquired lease. Expiration or a changed stored value can invalidate it.
// Operations compare ownership in Redis; possession of a Lock alone does not prove current ownership.
type Lock struct {
	client IRedisClient
	keys   []string
	// value is len(token) + ":" + token + metadata; the length header and token identify the owner.
	value string
	// ownerLen separates the ownership prefix from metadata, using byte offsets.
	ownerLen int
}

// Key returns the first key, or an empty string for a nil or empty Lock.
func (l *Lock) Key() string {
	if l == nil {
		return ""
	}
	if len(l.keys) == 0 {
		return ""
	}
	return l.keys[0]
}

// Keys returns a copy of the key list so callers cannot modify the lease's scope.
func (l *Lock) Keys() []string {
	if l == nil {
		return nil
	}
	keysCopy := make([]string, len(l.keys))
	copy(keysCopy, l.keys)
	return keysCopy
}

// Token returns the original owner token without its storage length header.
func (l *Lock) Token() string {
	if l == nil {
		return ""
	}
	if l.ownerLen <= 0 || l.ownerLen > len(l.value) {
		return ""
	}
	owner := l.value[:l.ownerLen]
	separator := strings.IndexByte(owner, ':')
	if separator < 0 {
		return ""
	}
	return owner[separator+1:]
}

// Metadata returns the metadata attached when this Lock was acquired.
func (l *Lock) Metadata() string {
	if l == nil {
		return ""
	}
	if l.ownerLen <= 0 || l.ownerLen > len(l.value) {
		return ""
	}
	return l.value[l.ownerLen:]
}

// Value returns the complete Redis value, including its length header.
// Use [Lock.Token] and [Lock.Metadata] to retrieve the original components.
func (l *Lock) Value() string {
	if l == nil {
		return ""
	}
	return l.value
}

// Refresh renews all keys if their values still match, otherwise returning [ErrLockNotHeld].
// ttl must be at least one millisecond and is rounded down to whole milliseconds.
func (l *Lock) Refresh(ctx context.Context, ttl time.Duration) error {
	if l == nil || nilClient(l.client) || len(l.keys) == 0 {
		return ErrLockNotHeld
	}
	if ttl < time.Millisecond {
		return errInvalidTTL
	}

	_, err := refreshScript.Run(ctx, l.client, l.keys, l.value, ttl.Milliseconds()).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ErrLockNotHeld
		}
		return err
	}
	return nil
}

// Release deletes all keys only if their values still match this Lock.
// An absent or changed value returns [ErrLockNotHeld].
func (l *Lock) Release(ctx context.Context) error {
	if l == nil || nilClient(l.client) || len(l.keys) == 0 {
		return ErrLockNotHeld
	}
	_, err := releaseScript.Run(ctx, l.client, l.keys, l.value).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ErrLockNotHeld
		}
		return err
	}
	return nil
}

// TTL returns the shortest positive TTL among matching keys.
// An absent or changed value returns zero with no error; a nil or empty Lock returns [ErrLockNotHeld].
func (l *Lock) TTL(ctx context.Context) (time.Duration, error) {
	if l == nil || nilClient(l.client) || len(l.keys) == 0 {
		return 0, ErrLockNotHeld
	}
	ttl, err := pttlScript.Run(ctx, l.client, l.keys, l.value).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, err
	}
	if ttl <= 0 {
		return 0, nil
	}
	return time.Duration(ttl) * time.Millisecond, nil
}
