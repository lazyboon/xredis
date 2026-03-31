package xredis

import (
	"context"
	_ "embed"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lazyboon/xretry"
	"github.com/redis/go-redis/v9"
)

type RedisClient interface {
	redis.Scripter
}

var (
	// ErrNotObtained indicates that a lock attempt failed because key(s) are already held.
	ErrNotObtained = errors.New("xredis: lock not obtained")

	// ErrLockNotHeld indicates that the lock is not currently held by the caller.
	ErrLockNotHeld = errors.New("xredis: lock not held")

	// Internal lock validation / protocol errors.
	errEmptyLockKeys  = errors.New("xredis: lock keys cannot be empty")
	errEmptyLockKey   = errors.New("xredis: lock key cannot be empty")
	errInvalidTTL     = errors.New("xredis: lock ttl must be greater than zero")
	errNilLockManager = errors.New("xredis: lock manager is nil")
	errNilRedisClient = errors.New("xredis: redis client is nil")
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

type LockOption func(*LockOptions)

// LockOptions configures lock obtain behavior.
type LockOptions struct {
	// Token identifies the lock owner. If nil/empty, a random UUID token is used.
	Token *string
	// Metadata is appended to the token and stored as lock value.
	Metadata *string
	// RetryStrategy controls retries when lock is contended. Default: no retry.
	RetryStrategy xretry.Strategy
}

// NewLockOption returns a lock option container with defaults.
func NewLockOption() *LockOptions {
	return &LockOptions{}
}

// Apply applies functional options to the receiver and returns it.
func (o *LockOptions) Apply(options ...LockOption) *LockOptions {
	for _, option := range options {
		if option != nil {
			option(o)
		}
	}
	return o
}

// WithLockToken sets a custom lock token.
func WithLockToken(token string) LockOption {
	return func(opts *LockOptions) {
		opts.Token = &token
	}
}

// WithLockMetadata sets lock metadata appended to the token.
func WithLockMetadata(metadata string) LockOption {
	return func(opts *LockOptions) {
		opts.Metadata = &metadata
	}
}

// WithLockRetryStrategy sets the retry strategy used by Obtain/ObtainMulti.
func WithLockRetryStrategy(strategy xretry.Strategy) LockOption {
	return func(opts *LockOptions) {
		opts.RetryStrategy = strategy
	}
}

// LockManager wraps a redis client and provides distributed lock operations.
type LockManager struct {
	client RedisClient
}

// NewLockManager creates a lock manager for the provided redis client.
func NewLockManager(c RedisClient) *LockManager {
	return &LockManager{client: c}
}

// Obtain tries to acquire a single-key lock.
// It returns ErrNotObtained when the lock cannot be acquired.
func (m *LockManager) Obtain(ctx context.Context, key string, ttl time.Duration, options ...LockOption) (*Lock, error) {
	return m.ObtainMulti(ctx, []string{key}, ttl, options...)
}

// ObtainMulti tries to acquire an atomic multi-key lock.
// If any key is contended, no lock is acquired and ErrNotObtained is returned.
func (m *LockManager) ObtainMulti(ctx context.Context, keys []string, ttl time.Duration, options ...LockOption) (*Lock, error) {
	if m == nil {
		return nil, errNilLockManager
	}
	if m.client == nil {
		return nil, errNilRedisClient
	}

	if err := m.validateObtainArgs(keys, ttl); err != nil {
		return nil, err
	}

	opts := NewLockOption().Apply(options...)
	token := m.resolveToken(opts.Token)
	value := token
	if opts.Metadata != nil {
		value = token + *opts.Metadata
	}

	runCtx := ctx
	cancel := func() {}
	// Keep retry bounded: when caller does not set a deadline,
	// cap attempts to at most one TTL window.
	if _, ok := ctx.Deadline(); !ok {
		runCtx, cancel = context.WithDeadline(ctx, time.Now().Add(ttl))
	}
	defer cancel()

	err := xretry.DoActionWithContext(
		runCtx,
		m.resolveRetryStrategy(opts),
		func(attemptCtx context.Context) error {
			return m.obtainOnce(attemptCtx, keys, value, len(token), ttl)
		},
		xretry.WithShouldRetry(func(_ context.Context, _ int, err error) bool {
			return errors.Is(err, ErrNotObtained)
		}),
	)
	if err != nil {
		return nil, m.unwrapRetryError(err)
	}

	return m.newLock(keys, value, len(token), ttl), nil
}

func (m *LockManager) validateObtainArgs(keys []string, ttl time.Duration) error {
	if len(keys) == 0 {
		return errEmptyLockKeys
	}
	for _, key := range keys {
		if key == "" {
			return errEmptyLockKey
		}
	}
	if ttl <= 0 {
		return errInvalidTTL
	}
	return nil
}

func (m *LockManager) resolveToken(token *string) string {
	if token != nil && *token != "" {
		return *token
	}
	return uuid.NewString()
}

func (m *LockManager) resolveRetryStrategy(opts *LockOptions) xretry.Strategy {
	if opts.RetryStrategy != nil {
		return opts.RetryStrategy
	}
	return xretry.NewZero()
}

func (m *LockManager) obtainOnce(ctx context.Context, keys []string, value string, tokenLen int, ttl time.Duration) error {
	_, err := obtainScript.Run(ctx, m.client, keys, value, tokenLen, ttl.Milliseconds()).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ErrNotObtained
		}
		return err
	}
	return nil
}

func (m *LockManager) unwrapRetryError(err error) error {
	var retryErr *xretry.RetryError
	// Collapse xretry wrapper to the concrete last error for cleaner API behavior.
	if errors.As(err, &retryErr) && retryErr.Last() != nil {
		return retryErr.Last()
	}
	return err
}

func (m *LockManager) newLock(keys []string, value string, tokenLen int, ttl time.Duration) *Lock {
	keysCopy := make([]string, len(keys))
	copy(keysCopy, keys)
	return &Lock{
		LockManager: m,
		keys:        keysCopy,
		value:       value,
		tokenLen:    tokenLen,
		ttl:         ttl,
	}
}

// ---------------------------------------------------------------------------------------------------------------------

type Lock struct {
	*LockManager
	keys []string
	// value is the exact payload stored in Redis (token + metadata).
	value string
	// tokenLen allows splitting token/metadata without storing them twice.
	tokenLen int
	ttl      time.Duration
}

// Key returns the first lock key.
func (l *Lock) Key() string {
	if l == nil {
		return ""
	}
	if len(l.keys) == 0 {
		return ""
	}
	return l.keys[0]
}

// Keys returns a copy of all lock keys.
func (l *Lock) Keys() []string {
	if l == nil {
		return nil
	}
	keysCopy := make([]string, len(l.keys))
	copy(keysCopy, l.keys)
	return keysCopy
}

// Token returns the lock token prefix from the stored lock value.
func (l *Lock) Token() string {
	if l == nil {
		return ""
	}
	if l.tokenLen <= 0 || l.tokenLen > len(l.value) {
		return ""
	}
	return l.value[:l.tokenLen]
}

// Metadata returns the metadata suffix from the stored lock value.
func (l *Lock) Metadata() string {
	if l == nil {
		return ""
	}
	if l.tokenLen <= 0 || l.tokenLen > len(l.value) {
		return ""
	}
	return l.value[l.tokenLen:]
}

// Value returns the exact value stored in Redis for this lock.
func (l *Lock) Value() string {
	if l == nil {
		return ""
	}
	return l.value
}

// Refresh extends the lock TTL for all keys.
// It returns ErrNotObtained if the lock is no longer held.
func (l *Lock) Refresh(ctx context.Context, ttl time.Duration) error {
	if l == nil {
		return ErrNotObtained
	}
	if ttl <= 0 {
		return errInvalidTTL
	}

	_, err := refreshScript.Run(ctx, l.client, l.keys, l.value, ttl.Milliseconds()).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ErrNotObtained
		}
		return err
	}
	l.ttl = ttl
	return nil
}

// Release deletes lock keys if the stored lock value still matches.
// It returns ErrLockNotHeld if the lock is no longer active.
func (l *Lock) Release(ctx context.Context) error {
	if l == nil {
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

// TTL returns the remaining lock TTL.
// For multi-key locks, the minimum TTL across keys is returned.
func (l *Lock) TTL(ctx context.Context) (time.Duration, error) {
	if l == nil {
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
