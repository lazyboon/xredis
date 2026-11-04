package xredis

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestLockAccessors(t *testing.T) {
	for _, token := range []string{"abc", "owner:with:colons", "\x00\xff:owner"} {
		t.Run(strconv.Quote(token), func(t *testing.T) {
			owner := strconv.Itoa(len(token)) + ":" + token
			lock := &Lock{keys: []string{"first", "second"}, value: owner + "metadata", ownerLen: len(owner)}
			if lock.Token() != token || lock.Metadata() != "metadata" || lock.Value() != owner+"metadata" {
				t.Fatal("owner framing lost token or metadata")
			}
			keys := lock.Keys()
			keys[0] = "modified"
			if lock.Key() != "first" {
				t.Fatal("caller mutated lock keys")
			}
		})
	}
	var missing *Lock
	if missing.Token() != "" || missing.Metadata() != "" || missing.Value() != "" || missing.Key() != "" {
		t.Fatal("nil lock accessors changed")
	}
	if err := missing.Release(context.Background()); !errors.Is(err, ErrLockNotHeld) {
		t.Fatal(err)
	}
	if _, err := missing.TTL(context.Background()); !errors.Is(err, ErrLockNotHeld) {
		t.Fatal(err)
	}
}

func TestLockArgumentValidation(t *testing.T) {
	manager := NewLockManager(nil)
	for _, keys := range [][]string{nil, {""}, {"same", "same"}} {
		if err := manager.validateObtainArgs(keys, time.Second); err == nil {
			t.Fatalf("invalid keys accepted: %v", keys)
		}
	}
	if err := manager.validateObtainArgs([]string{"key"}, time.Microsecond); err == nil {
		t.Fatal("sub-millisecond lease accepted")
	}
	var absent *LockManager
	if _, err := absent.Obtain(context.Background(), "key", time.Second); err == nil {
		t.Fatal("nil manager accepted")
	}
	if _, err := manager.Obtain(context.Background(), "key", time.Second); err == nil {
		t.Fatal("nil client accepted")
	}
}
