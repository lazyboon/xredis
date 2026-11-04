package xredis

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

type limitCase struct {
	name  string
	limit Limit
	valid bool
}

func TestLimitValidate(t *testing.T) {
	cases := []limitCase{
		{name: "zero", limit: Limit{}},
		{name: "negative rate", limit: NewLimit(-1, time.Second)},
		{name: "sub-microsecond period", limit: NewLimit(1, time.Nanosecond)},
		{name: "unrepresentable increment", limit: NewLimit(100_000_000, time.Second, 1000)},
		{name: "zero burst", limit: NewLimit(1, time.Second, 0)},
		{name: "negative burst", limit: NewLimit(1, time.Second, -1)},
		{name: "recovery overflow", limit: NewLimit(1, time.Hour, int(^uint(0)>>1))},
		{name: "precision boundary", limit: NewLimit(1_000_000, time.Second), valid: true},
		{name: "ordinary", limit: NewPerMinuteLimit(120, 5), valid: true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			err := item.limit.Validate()
			if (err == nil) != item.valid {
				t.Fatalf("validation = %v, valid = %t", err, item.valid)
			}
			if err != nil && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("missing input category: %v", err)
			}
		})
	}
}

func TestLimitConstructors(t *testing.T) {
	if got := NewPerSecondLimit(3); got != (Limit{Rate: 3, Burst: 3, Period: time.Second}) {
		t.Fatalf("unexpected default burst: %v", got)
	}
	if got := NewPerDayLimit(5, 2); got.Period != 24*time.Hour || got.Burst != 2 {
		t.Fatal(got)
	}
	if !(Limit{}).IsZero() || NewPerHourLimit(1).IsZero() {
		t.Fatal("zero rule detection changed")
	}
	if got := NewPerMinuteLimit(5).String(); got != "5 req/m (burst 5)" {
		t.Fatal(got)
	}
}

type protocolCase struct {
	name  string
	value any
}

func TestLimiterScriptProtocol(t *testing.T) {
	limiter := NewLimiter(nil)
	result, err := limiter.parseMultiRateResult([]any{int64(2), int64(3), int64(-1), int64(10), int64(2)})
	if err != nil || result.Allowed != 2 || result.LimitedBy.Index != 1 || result.ResetAfter != 10*time.Microsecond {
		t.Fatalf("valid admission: %+v / %v", result, err)
	}
	result, err = limiter.parseMultiRateResult([]any{"0", "0", "2500", "5000", "1"})
	if err != nil || result.RetryAfter != 2500*time.Microsecond || result.LimitedBy.Index != 0 {
		t.Fatalf("valid denial: %+v / %v", result, err)
	}
	cases := []protocolCase{
		{name: "nil", value: nil},
		{name: "wrong tuple length", value: []any{int64(1)}},
		{name: "fractional count", value: []any{1.5, int64(0), int64(-1), int64(0), int64(-1)}},
		{name: "negative count", value: []any{int64(-1), int64(0), int64(-1), int64(0), int64(-1)}},
		{name: "invalid wait", value: []any{int64(1), int64(0), int64(0), int64(0), int64(-1)}},
		{name: "denial without wait", value: []any{int64(0), int64(0), int64(-1), int64(0), int64(-1)}},
		{name: "invalid index", value: []any{int64(1), int64(0), int64(-1), int64(0), int64(0)}},
		{name: "duration overflow", value: []any{int64(0), int64(0), int64(math.MaxInt64), int64(0), int64(1)}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if _, err := limiter.parseMultiRateResult(item.value); !errors.Is(err, ErrScriptProtocol) {
				t.Fatalf("invalid protocol accepted: %v", err)
			}
		})
	}
}

func TestLimiterMissingClient(t *testing.T) {
	var limiter *Limiter
	if _, err := limiter.Allow(context.Background(), "key", NewPerSecondLimit(1)); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if err := NewLimiter(nil).Reset(context.Background(), "key"); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
}

func FuzzLimiterScriptProtocol(f *testing.F) {
	f.Add(int64(1), int64(0), int64(-1), int64(1), int64(-1))
	f.Add(int64(0), int64(0), int64(1000), int64(1000), int64(1))
	f.Fuzz(func(t *testing.T, allowed, remaining, retry, reset, index int64) {
		result, err := NewLimiter(nil).parseMultiRateResult([]any{allowed, remaining, retry, reset, index})
		if err == nil && (result.Allowed < 0 || result.Remaining < 0 || result.ResetAfter < 0) {
			t.Fatalf("unsafe protocol result: %+v", result)
		}
	})
}
