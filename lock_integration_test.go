//go:build integration

package xredis_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lazyboon/xredis"
	"github.com/lazyboon/xretry"
)

func TestLockOwnerRetryAndMetadata(t *testing.T) {
	client, prefix := integrationClient(t)
	ctx := t.Context()
	manager := xredis.NewLockManager(client)
	keys := []string{prefix + ":a", prefix + ":b"}
	token := "owner:with:\x00\xff"
	first, err := manager.ObtainMulti(ctx, keys, time.Minute,
		xredis.WithLockToken(token), xredis.WithLockMetadata("first"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Token() != token || first.Metadata() != "first" {
		t.Fatal("binary ownership token was not preserved")
	}
	if _, err := manager.ObtainMulti(ctx, keys, time.Minute, xredis.WithLockToken("owner")); !errors.Is(err, xredis.ErrNotObtained) {
		t.Fatalf("short owner prefix acquired a different lease: %v", err)
	}
	next, err := manager.ObtainMulti(ctx, keys, time.Minute,
		xredis.WithLockToken(token), xredis.WithLockMetadata("second"))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx); !errors.Is(err, xredis.ErrLockNotHeld) {
		t.Fatalf("old metadata released replacement: %v", err)
	}
	if err := first.Refresh(ctx, time.Minute); !errors.Is(err, xredis.ErrLockNotHeld) {
		t.Fatalf("old metadata renewed replacement: %v", err)
	}
	if err := next.Refresh(ctx, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if ttl, err := next.TTL(ctx); err != nil || ttl < time.Minute || ttl > 2*time.Minute {
		t.Fatalf("renewed TTL = %v / %v", ttl, err)
	}
	if err := next.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if ttl, err := next.TTL(ctx); err != nil || ttl != 0 {
		t.Fatalf("released TTL = %v / %v", ttl, err)
	}
}

func TestLockMultiKeyAcquisitionIsAtomic(t *testing.T) {
	client, prefix := integrationClient(t)
	ctx := t.Context()
	manager := xredis.NewLockManager(client)
	first, err := manager.Obtain(ctx, prefix+":held", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ObtainMulti(ctx, []string{prefix + ":held", prefix + ":free"}, time.Minute); !errors.Is(err, xredis.ErrNotObtained) {
		t.Fatal(err)
	}
	exists, err := client.Exists(ctx, prefix+":free").Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("failed acquisition consumed a free key")
	}
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}
	winners := make(chan *xredis.Lock, 20)
	var wait sync.WaitGroup
	for range 20 {
		wait.Go(func() {
			lock, err := manager.ObtainMulti(ctx, []string{prefix + ":a", prefix + ":b"}, time.Minute)
			if err == nil {
				winners <- lock
			} else if !errors.Is(err, xredis.ErrNotObtained) {
				t.Errorf("unexpected contention error: %v", err)
			}
		})
	}
	wait.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("winners = %d", len(winners))
	}
	if err := (<-winners).Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLockExpiryAndRetryCancellation(t *testing.T) {
	client, prefix := integrationClient(t)
	ctx := t.Context()
	manager := xredis.NewLockManager(client)
	first, err := manager.Obtain(ctx, prefix, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		exists, err := client.Exists(ctx, prefix).Result()
		if err != nil {
			t.Fatal(err)
		}
		if exists == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease did not expire")
		}
		time.Sleep(5 * time.Millisecond)
	}
	next, err := manager.Obtain(ctx, prefix, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx); !errors.Is(err, xredis.ErrLockNotHeld) {
		t.Fatalf("expired owner deleted successor: %v", err)
	}
	retryCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	_, err = manager.Obtain(retryCtx, prefix, time.Minute,
		xredis.WithLockRetryStrategy(xretry.NewLinear(5*time.Millisecond)))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retry ignored caller deadline: %v", err)
	}
	if err := next.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
