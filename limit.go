package xredis

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua uses double-precision numbers, which cannot represent larger integers exactly.
const maxLuaInteger = (1 << 53) - 1

var (
	//go:embed lua/rate_allow_multi_n.lua
	allowMultiNLua    string
	allowMultiNScript = redis.NewScript(allowMultiNLua)

	//go:embed lua/rate_allow_multi_at_most.lua
	allowMultiAtMostLua    string
	allowMultiAtMostScript = redis.NewScript(allowMultiAtMostLua)
)

var (
	// ErrInvalidInput indicates an invalid rate limit, key, or request cost.
	ErrInvalidInput = errors.New("xredis: invalid input")
	// ErrScriptProtocol indicates an unexpected script result or an unrepresentable result duration.
	ErrScriptProtocol = errors.New("xredis: invalid script result")
)

// IRateClient is the script execution and deletion contract required by Limiter.
type IRateClient interface {
	redis.Scripter
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// Limit defines a continuously replenished rate limit for one key.
// Rules using the same key share state; use Reset or a new key when changing a rule's parameters.
type Limit struct {
	Rate   int           // Requests replenished per Period; positive, with a minimum interval of 1us.
	Burst  int           // Maximum immediately available quota; must be positive.
	Period time.Duration // Replenishment period, truncated to microseconds; must be at least one microsecond.
}

// String formats the rate, period, and burst capacity for diagnostics.
func (l Limit) String() string {
	return fmt.Sprintf("%d req/%s (burst %d)", l.Rate, l.formatPeriod(), l.Burst)
}

// IsZero reports whether all fields are unset; a zero Limit is not a valid rule.
func (l Limit) IsZero() bool {
	return l == Limit{}
}

// formatPeriod abbreviates common periods for diagnostic output.
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

// NewLimit constructs a rule, defaulting burst capacity to rate.
// If supplied, only the first burst argument is used. Call [Limit.Validate] to check the rule.
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

// NewPerSecondLimit constructs a rule with a one-second replenishment period.
func NewPerSecondLimit(rate int, burst ...int) Limit {
	return NewLimit(rate, time.Second, burst...)
}

// NewPerMinuteLimit constructs a rule with a one-minute replenishment period.
func NewPerMinuteLimit(rate int, burst ...int) Limit {
	return NewLimit(rate, time.Minute, burst...)
}

// NewPerHourLimit constructs a rule with a one-hour replenishment period.
func NewPerHourLimit(rate int, burst ...int) Limit {
	return NewLimit(rate, time.Hour, burst...)
}

// NewPerDayLimit constructs a rule with a fixed 24-hour replenishment period.
func NewPerDayLimit(rate int, burst ...int) Limit {
	return NewLimit(rate, time.Hour*24, burst...)
}

// Validate checks positive parameters, Lua integer precision, and the maximum recovery duration.
func (l Limit) Validate() error {
	if l.Rate <= 0 {
		return invalidInputf("rate must be > 0")
	}
	if l.Period < time.Microsecond {
		return invalidInputf("period must be >= 1us")
	}
	// TAT is an absolute Unix timestamp in Lua. Sub-microsecond increments can
	// disappear when added to that timestamp, leaving admitted requests unrecorded.
	if int64(l.Rate) > l.Period.Microseconds() {
		return invalidInputf("replenishment interval must be >= 1us")
	}
	if l.Burst <= 0 {
		return invalidInputf("burst must be > 0")
	}
	if max(int64(l.Rate), int64(l.Burst), l.Period.Microseconds()) > maxLuaInteger {
		return invalidInputf("rule exceeds the exact integer range of Lua")
	}
	return l.validateCost(l.Burst)
}

// validateCost checks a positive cost's recovery or retry duration before executing a script.
func (l Limit) validateCost(cost int) error {
	// Divide a 128-bit product to compare period * cost / rate without intermediate overflow.
	// Callers first validate positivity and the Lua integer bounds of all inputs.
	hi, lo := bits.Mul64(uint64(l.Period.Microseconds()), uint64(cost))
	divisor := uint64(l.Rate)
	if hi >= divisor {
		return invalidInputf("cost recovery time exceeds time.Duration range")
	}
	microseconds, remainder := bits.Div64(hi, lo, divisor)
	const maxDurationMicros = uint64(math.MaxInt64 / int64(time.Microsecond))
	if microseconds > maxDurationMicros || (microseconds == maxDurationMicros && remainder != 0) {
		return invalidInputf("cost recovery time exceeds time.Duration range")
	}
	return nil
}

// LimitedBy identifies the rule that denied or partially constrained a request.
type LimitedBy struct {
	// Index is the zero-based rule index, or -1 if no rule constrained the request.
	Index int
	// Key is the constraining rule's Redis key, or empty if Index is -1.
	Key string
}

// Result describes a rate-limit decision. Denial is a successful decision, not an error.
type Result struct {
	// Allowed is the number of requests admitted by this call.
	Allowed int
	// Remaining is the minimum remaining quota across rules after admission; it is zero on denial.
	Remaining int
	// RetryAfter is the wait before retrying a denied request; -1 means this call admitted requests.
	RetryAfter time.Duration
	// ResetAfter is the reported recovery duration: across all rules on admission, or the selected rule on denial.
	ResetAfter time.Duration
	// LimitedBy identifies a denying or partially constraining rule; Index -1 means none.
	LimitedBy LimitedBy
}

// Rule associates a Redis key with its rate-limit parameters.
type Rule struct {
	Key   string // Nonempty Redis key; each key must appear only once in a multi-rule request.
	Limit Limit  // Rate-limit parameters shared by all requests using Key.
}

// Limiter checks and updates quotas using atomic Redis scripts and Redis server time.
// It can be shared if its client supports concurrent use. The caller retains client ownership.
type Limiter struct {
	rdb IRateClient
}

// NewLimiter creates a limiter without opening a connection or writing quota state.
func NewLimiter(rdb IRateClient) *Limiter {
	return &Limiter{rdb: rdb}
}

// Allow checks whether one request can consume quota under a rule.
func (l *Limiter) Allow(ctx context.Context, key string, limit Limit) (*Result, error) {
	return l.AllowN(ctx, key, limit, 1)
}

// AllowN admits all n requests or none. A denial returns a Result with Allowed 0 and no error.
// RetryAfter describes the wait for the full cost; n greater than Burst can never be admitted by this rule.
func (l *Limiter) AllowN(ctx context.Context, key string, limit Limit, n int) (*Result, error) {
	if key == "" {
		return nil, invalidInputf("key must not be empty")
	}
	return l.AllowNRules(ctx, []Rule{{Key: key, Limit: limit}}, n)
}

// AllowAtMost admits as many of n requests as the rule currently permits.
// On denial, RetryAfter is the wait until at least one request can be admitted.
func (l *Limiter) AllowAtMost(ctx context.Context, key string, limit Limit, n int) (*Result, error) {
	if key == "" {
		return nil, invalidInputf("key must not be empty")
	}
	return l.AllowAtMostRules(ctx, []Rule{{Key: key, Limit: limit}}, n)
}

// AllowRules checks one request against all rules without consuming quota if any rule denies it.
func (l *Limiter) AllowRules(ctx context.Context, rules []Rule) (*Result, error) {
	return l.AllowNRules(ctx, rules, 1)
}

// AllowNRules admits all n requests only when every rule permits them.
// Rule keys must be unique and must share a hash slot when using Redis Cluster.
// The script consumes no quota if any rule denies the request.
func (l *Limiter) AllowNRules(ctx context.Context, rules []Rule, n int) (*Result, error) {
	return l.runMultiScript(ctx, rules, n, allowMultiNScript)
}

// AllowAtMostRules admits the largest cost up to n that every rule currently permits.
// Rule keys must be unique and must share a hash slot when using Redis Cluster.
func (l *Limiter) AllowAtMostRules(ctx context.Context, rules []Rule, n int) (*Result, error) {
	return l.runMultiScript(ctx, rules, n, allowMultiAtMostScript)
}

// runMultiScript validates inputs, executes a multi-rule script, and checks the response protocol.
func (l *Limiter) runMultiScript(ctx context.Context, rules []Rule, n int, script *redis.Script) (*Result, error) {
	if l == nil || nilClient(l.rdb) {
		return nil, invalidInputf("redis client is nil")
	}
	if n <= 0 {
		return nil, invalidInputf("n must be > 0")
	}
	if int64(n) > maxLuaInteger {
		return nil, invalidInputf("n exceeds the exact integer range of Lua")
	}
	keys, values, err := l.buildMultiScriptArgs(rules)
	if err != nil {
		return nil, err
	}
	// A full-cost retry interval may exceed the burst recovery interval and needs its own check.
	// Partial admission never exceeds Burst, whose recovery range was checked by Validate.
	if script == allowMultiNScript {
		for i, rule := range rules {
			if err = rule.Limit.validateCost(n); err != nil {
				return nil, fmt.Errorf("invalid rule %d: %w", i, err)
			}
		}
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

// buildMultiScriptArgs validates rules and converts their periods to script microseconds.
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

// Reset deletes quota state for the given keys. Missing keys are ignored.
// In Redis Cluster, all keys in one call must share a hash slot.
func (l *Limiter) Reset(ctx context.Context, keys ...string) error {
	if l == nil || nilClient(l.rdb) {
		return invalidInputf("redis client is nil")
	}
	if len(keys) == 0 {
		return invalidInputf("keys must not be empty")
	}
	if slices.Contains(keys, "") {
		return invalidInputf("key must not be empty")
	}
	return l.rdb.Del(ctx, keys...).Err()
}

// toInt converts script integers while checking the platform's int range.
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

// toInt64 accepts integer representations returned by supported Redis clients.
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

// microsecondsToDuration converts a script duration after its range has been checked.
func microsecondsToDuration(v int64) time.Duration {
	return time.Duration(v) * time.Microsecond
}

// retryAfterMicrosecondsToDuration preserves -1 as the no-wait sentinel.
func retryAfterMicrosecondsToDuration(v int64) time.Duration {
	if v == -1 {
		return -1
	}
	return microsecondsToDuration(v)
}

// parseMultiRateResult validates the script protocol and duration ranges before constructing a Result.
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
	const maxDurationMicros = math.MaxInt64 / int64(time.Microsecond)
	if retryAfterMicros > maxDurationMicros || resetAfterMicros > maxDurationMicros {
		return nil, scriptProtocolf("duration exceeds time.Duration range")
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
		limitedByIndex-- // Convert Lua's one-based rule index to Go's zero-based index.
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

// invalidInputf wraps [ErrInvalidInput] with argument-specific details.
func invalidInputf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

// scriptProtocolf wraps [ErrScriptProtocol] with response-specific details.
func scriptProtocolf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrScriptProtocol, fmt.Sprintf(format, args...))
}
