package xredis

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	//go:embed lua/rate_allow_multi_n.lua
	allowMultiNLua    string
	allowMultiNScript = redis.NewScript(allowMultiNLua)

	//go:embed lua/rate_allow_multi_at_most.lua
	allowMultiAtMostLua    string
	allowMultiAtMostScript = redis.NewScript(allowMultiAtMostLua)
)

var (
	// ErrInvalidInput indicates caller-provided arguments are invalid.
	ErrInvalidInput = errors.New("xredis: invalid input")
	// ErrScriptProtocol indicates the Lua->Go script result protocol is invalid.
	ErrScriptProtocol = errors.New("xredis: invalid script result")
)

// ---------------------------------------------------------------------------------------------------------------------

// RateClient is the minimal redis client contract required by Limiter.
type RateClient interface {
	redis.Scripter
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// ---------------------------------------------------------------------------------------------------------------------

// Limit defines a burstable rate limit for one key.
type Limit struct {
	Rate   int
	Burst  int
	Period time.Duration
}

func (l Limit) String() string {
	return fmt.Sprintf("%d req/%s (burst %d)", l.Rate, l.formatPeriod(), l.Burst)
}

func (l Limit) IsZero() bool {
	return l == Limit{}
}

func (l Limit) formatPeriod() string {
	switch l.Period {
	case time.Second:
		return "s"
	case time.Minute:
		return "m"
	case time.Hour:
		return "h"
	case time.Hour * 24:
		return "d"
	default:
		return l.Period.String()
	}
}

// NewLimit returns a rate limit for the given rate and period.
// If burst is not provided, it defaults to rate.
func NewLimit(rate int, period time.Duration, burst ...int) Limit {
	b := rate
	if len(burst) > 0 {
		b = burst[0]
	}
	return Limit{
		Rate:   rate,
		Burst:  b,
		Period: period,
	}
}

func NewPerSecondLimit(rate int, burst ...int) Limit {
	return NewLimit(rate, time.Second, burst...)
}

func NewPerMinuteLimit(rate int, burst ...int) Limit {
	return NewLimit(rate, time.Minute, burst...)
}

func NewPerHourLimit(rate int, burst ...int) Limit {
	return NewLimit(rate, time.Hour, burst...)
}

func NewPerDayLimit(rate int, burst ...int) Limit {
	return NewLimit(rate, time.Hour*24, burst...)
}

func (l Limit) Validate() error {
	if l.Rate <= 0 {
		return invalidInputf("rate must be > 0")
	}
	if l.Period <= 0 {
		return invalidInputf("period must be > 0")
	}
	if l.Burst < 0 {
		return invalidInputf("burst must be >= 0")
	}
	return nil
}

// ---------------------------------------------------------------------------------------------------------------------

// LimitedBy identifies the rule that constrained a decision.
type LimitedBy struct {
	// Index is the zero-based index in rules. -1 means none.
	Index int
	// Key is rules[Index].Key when Index is valid.
	Key string
}

// Result is the outcome of one limiter decision.
type Result struct {
	// Allowed is the amount accepted for this call.
	Allowed int
	// Remaining is the post-update remaining allowance after applying Allowed.
	Remaining int
	// RetryAfter is the wait duration before retrying.
	// It is -1 when no retry delay is required.
	RetryAfter time.Duration
	// ResetAfter is the duration until the limiter state is fully drained/reset.
	ResetAfter time.Duration
	// LimitedBy describes which rule constrained the decision.
	// LimitedBy.Index is -1 when no specific limiter constrained (e.g. fully allowed).
	LimitedBy LimitedBy
}

// Rule defines one redis key and its rate limit.
type Rule struct {
	Key   string
	Limit Limit
}

// ---------------------------------------------------------------------------------------------------------------------

// Limiter controls how frequently events are allowed to happen.
type Limiter struct {
	rdb RateClient
}

// NewLimiter returns a new rate limiter.
func NewLimiter(rdb RateClient) *Limiter {
	return &Limiter{rdb: rdb}
}

// Allow is a shortcut for AllowN(ctx, key, limit, 1).
func (l *Limiter) Allow(ctx context.Context, key string, limit Limit) (*Result, error) {
	return l.AllowN(ctx, key, limit, 1)
}

// AllowN reports whether n events may happen at time now.
func (l *Limiter) AllowN(ctx context.Context, key string, limit Limit, n int) (*Result, error) {
	if key == "" {
		return nil, invalidInputf("key must not be empty")
	}
	return l.AllowNRules(ctx, []Rule{{Key: key, Limit: limit}}, n)
}

// AllowAtMost reports whether at most n events may happen at time now.
// It returns number of allowed events that is less than or equal to n.
func (l *Limiter) AllowAtMost(ctx context.Context, key string, limit Limit, n int) (*Result, error) {
	if key == "" {
		return nil, invalidInputf("key must not be empty")
	}
	return l.AllowAtMostRules(ctx, []Rule{{Key: key, Limit: limit}}, n)
}

// AllowRules is a shortcut for AllowNRules(ctx, rules, 1).
func (l *Limiter) AllowRules(ctx context.Context, rules []Rule) (*Result, error) {
	return l.AllowNRules(ctx, rules, 1)
}

// AllowNRules reports whether n events may happen at time now under all rules.
// The script checks all limits first, and only updates state when all limits pass.
func (l *Limiter) AllowNRules(ctx context.Context, rules []Rule, n int) (*Result, error) {
	return l.runMultiScript(ctx, rules, n, allowMultiNScript)
}

// AllowAtMostRules reports whether at most n events may happen at time now under all rules.
// It returns number of allowed events that is less than or equal to n.
func (l *Limiter) AllowAtMostRules(ctx context.Context, rules []Rule, n int) (*Result, error) {
	return l.runMultiScript(ctx, rules, n, allowMultiAtMostScript)
}

func (l *Limiter) runMultiScript(ctx context.Context, rules []Rule, n int, script *redis.Script) (*Result, error) {
	if n <= 0 {
		return nil, invalidInputf("n must be > 0")
	}
	keys, values, err := l.buildMultiScriptArgs(rules)
	if err != nil {
		return nil, err
	}
	values = append(values, n)
	v, err := script.Run(ctx, l.rdb, keys, values...).Result()
	if err != nil {
		return nil, err
	}
	res, err := l.parseMultiRateResult(v)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, scriptProtocolf("script returned nil result")
	}
	if res.LimitedBy.Index >= 0 {
		if res.LimitedBy.Index >= len(rules) {
			return nil, scriptProtocolf("limited_by_index out of range: %d", res.LimitedBy.Index)
		}
		res.LimitedBy.Key = rules[res.LimitedBy.Index].Key
	}
	return res, nil
}

func (l *Limiter) buildMultiScriptArgs(rules []Rule) ([]string, []any, error) {
	if len(rules) == 0 {
		return nil, nil, invalidInputf("rules must not be empty")
	}
	keys := make([]string, 0, len(rules))
	values := make([]any, 0, len(rules)*3+1)
	seen := make(map[string]int, len(rules))
	for i := range rules {
		key := rules[i].Key
		if key == "" {
			return nil, nil, invalidInputf("invalid rule %d: key must not be empty", i)
		}
		if prev, ok := seen[key]; ok {
			return nil, nil, invalidInputf("invalid rule %d: duplicated key %q (already used by rule %d)", i, key, prev)
		}
		seen[key] = i
		if err := rules[i].Limit.Validate(); err != nil {
			return nil, nil, fmt.Errorf("invalid rule %d: %w", i, err)
		}
		periodMicros := rules[i].Limit.Period.Microseconds()
		if periodMicros <= 0 {
			return nil, nil, invalidInputf("invalid rule %d: period must be >= 1us", i)
		}
		keys = append(keys, key)
		values = append(values, rules[i].Limit.Burst, rules[i].Limit.Rate, periodMicros)
	}
	return keys, values, nil
}

// Reset removes all rate limit state for one or more keys.
func (l *Limiter) Reset(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return invalidInputf("keys must not be empty")
	}
	for i := range keys {
		if keys[i] == "" {
			return invalidInputf("key must not be empty")
		}
	}
	return l.rdb.Del(ctx, keys...).Err()
}

func toInt(v any) (int, error) {
	maxInt := int64(^uint(0) >> 1)
	minInt := -maxInt - 1
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		if x > maxInt || x < minInt {
			return 0, fmt.Errorf("int64 %d overflows int", x)
		}
		return int(x), nil
	case string:
		i64, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("unexpected int string %q: %w", x, err)
		}
		if i64 > maxInt || i64 < minInt {
			return 0, fmt.Errorf("int string %q overflows int", x)
		}
		return int(i64), nil
	default:
		return 0, fmt.Errorf("unexpected int type %T", v)
	}
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case string:
		i, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("unexpected int64 string %q: %w", x, err)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("unexpected int64 type %T", v)
	}
}

func microsecondsToDuration(v int64) time.Duration {
	return time.Duration(v) * time.Microsecond
}

func retryAfterMicrosecondsToDuration(v int64) time.Duration {
	if v == -1 {
		return -1
	}
	return microsecondsToDuration(v)
}

func (l *Limiter) parseMultiRateResult(v any) (*Result, error) {
	values, ok := v.([]any)
	if !ok || len(values) != 5 {
		return nil, scriptProtocolf("unexpected multi rate script result type %T", v)
	}
	allowed, err := toInt(values[0])
	if err != nil {
		return nil, scriptProtocolf("invalid allowed value: %v", err)
	}
	remaining, err := toInt(values[1])
	if err != nil {
		return nil, scriptProtocolf("invalid remaining value: %v", err)
	}
	retryAfterMicros, err := toInt64(values[2])
	if err != nil {
		return nil, scriptProtocolf("invalid retry_after value: %v", err)
	}
	resetAfterMicros, err := toInt64(values[3])
	if err != nil {
		return nil, scriptProtocolf("invalid reset_after value: %v", err)
	}
	limitedByIndex, err := toInt(values[4])
	if err != nil {
		return nil, scriptProtocolf("invalid limited_by_index value: %v", err)
	}
	if allowed < 0 {
		return nil, scriptProtocolf("allowed must be >= 0")
	}
	if remaining < 0 {
		return nil, scriptProtocolf("remaining must be >= 0")
	}
	if retryAfterMicros < -1 {
		return nil, scriptProtocolf("retry_after must be >= -1")
	}
	if resetAfterMicros < 0 {
		return nil, scriptProtocolf("reset_after must be >= 0")
	}
	if limitedByIndex == 0 || limitedByIndex < -1 {
		return nil, scriptProtocolf("limited_by_index must be -1 or >= 1")
	}
	if retryAfterMicros == -1 && allowed == 0 {
		return nil, scriptProtocolf("retry_after cannot be -1 when allowed is 0")
	}
	if retryAfterMicros >= 0 && allowed > 0 {
		return nil, scriptProtocolf("retry_after must be -1 when allowed is > 0")
	}
	if limitedByIndex > 0 {
		limitedByIndex-- // lua index is 1-based, go index is 0-based.
	} else {
		limitedByIndex = -1
	}
	return &Result{
		Allowed:    allowed,
		Remaining:  remaining,
		RetryAfter: retryAfterMicrosecondsToDuration(retryAfterMicros),
		ResetAfter: microsecondsToDuration(resetAfterMicros),
		LimitedBy: LimitedBy{
			Index: limitedByIndex,
		},
	}, nil
}

func invalidInputf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

func scriptProtocolf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrScriptProtocol, fmt.Sprintf(format, args...))
}
