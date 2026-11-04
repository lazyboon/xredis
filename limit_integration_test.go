//go:build integration

package xredis_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lazyboon/xredis"
)

func TestLimiterMultiRuleAtomicityAndPartialAdmission(t *testing.T) {
	client, prefix := integrationClient(t)
	ctx := t.Context()
	limiter := xredis.NewLimiter(client)
	rules := []xredis.Rule{
		{Key: prefix + ":a", Limit: xredis.NewPerHourLimit(1, 2)},
		{Key: prefix + ":b", Limit: xredis.NewPerHourLimit(1, 1)},
	}
	result, err := limiter.AllowNRules(ctx, rules, 2)
	if err != nil || result.Allowed != 0 || result.LimitedBy.Index != 1 || result.RetryAfter <= 0 {
		t.Fatalf("full-cost denial = %+v / %v", result, err)
	}
	exists, err := client.Exists(ctx, rules[0].Key, rules[1].Key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("denial consumed quota")
	}
	result, err = limiter.AllowAtMostRules(ctx, rules, 3)
	if err != nil || result.Allowed != 1 || result.LimitedBy.Key != rules[1].Key {
		t.Fatalf("partial admission = %+v / %v", result, err)
	}
	before, err := client.Get(ctx, rules[0].Key).Result()
	if err != nil {
		t.Fatal(err)
	}
	result, err = limiter.AllowRules(ctx, rules)
	if err != nil || result.Allowed != 0 {
		t.Fatalf("exhausted rule accepted: %+v / %v", result, err)
	}
	after, err := client.Get(ctx, rules[0].Key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("failed multi-rule check changed another rule")
	}
	ttl, err := client.PTTL(ctx, rules[0].Key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 || ttl > 2*time.Hour {
		t.Fatalf("quota TTL = %v", ttl)
	}
	if err := limiter.Reset(ctx, rules[0].Key, rules[1].Key); err != nil {
		t.Fatal(err)
	}
	result, err = limiter.AllowRules(ctx, rules)
	if err != nil || result.Allowed != 1 {
		t.Fatalf("reset did not restore quota: %+v / %v", result, err)
	}
}

func TestLimiterConcurrentQuota(t *testing.T) {
	client, prefix := integrationClient(t)
	limiter := xredis.NewLimiter(client)
	var admitted atomic.Int32
	var wait sync.WaitGroup
	for range 32 {
		wait.Go(func() {
			result, err := limiter.Allow(t.Context(), prefix, xredis.NewPerHourLimit(1, 10))
			if err != nil {
				t.Error(err)
				return
			}
			admitted.Add(int32(result.Allowed))
		})
	}
	wait.Wait()
	if admitted.Load() != 10 {
		t.Fatalf("quota consumed %d times", admitted.Load())
	}
}

func TestLimiterRejectsUnrepresentableRuleBeforeMutation(t *testing.T) {
	client, prefix := integrationClient(t)
	limiter := xredis.NewLimiter(client)
	_, err := limiter.Allow(t.Context(), prefix, xredis.NewPerSecondLimit(100_000_000, 1000))
	if !errors.Is(err, xredis.ErrInvalidInput) {
		t.Fatal(err)
	}
	exists, err := client.Exists(t.Context(), prefix).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("invalid rule changed quota")
	}
}
