package xredis_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lazyboon/xredis"
	"github.com/redis/go-redis/v9"
)

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("bad array %q", line)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		line, err = r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		n, err = strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			return nil, err
		}
		data := make([]byte, n+2)
		if _, err = io.ReadFull(r, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:n])
	}
	return args, nil
}

func stubConfig(alias string, stall string) (*xredis.Config, <-chan struct{}, <-chan struct{}, func()) {
	entered := make(chan struct{})
	closed := make(chan struct{})
	var mu sync.Mutex
	var peers []net.Conn
	var enteredOnce, closedOnce sync.Once
	var handlers sync.WaitGroup
	cfg := &xredis.Config{Alias: alias, Addr: "audit.invalid:6379", ExtraOptions: &redis.Options{
		Protocol: 2, DisableIdentity: true, ReadTimeout: -2, WriteTimeout: -2, MaxRetries: -1,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			mu.Lock()
			peers = append(peers, client, server)
			mu.Unlock()
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer closedOnce.Do(func() { close(closed) })
				defer server.Close()
				r := bufio.NewReader(server)
				for {
					args, err := readCommand(r)
					if err != nil {
						return
					}
					command := strings.ToLower(args[0])
					if command == stall {
						enteredOnce.Do(func() { close(entered) })
						_, _ = io.Copy(io.Discard, r)
						return
					}
					reply := "+OK\r\n"
					if command == "hello" {
						reply = "-ERR unknown command 'hello'\r\n"
					} else if command == "ping" {
						reply = "+PONG\r\n"
					}
					if _, err = io.WriteString(server, reply); err != nil {
						return
					}
				}
			}()
			return client, nil
		},
	}}
	return cfg, entered, closed, func() {
		mu.Lock()
		for _, peer := range peers {
			_ = peer.Close()
		}
		mu.Unlock()
		handlers.Wait()
	}
}

func TestStartupDeadlines(t *testing.T) {
	for _, stall := range []string{"hello", "ping"} {
		for _, parent := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/parent=%t", stall, parent), func(t *testing.T) {
				cfg, entered, closed, cleanup := stubConfig("timeout-"+stall, stall)
				defer cleanup()
				timeout := uint(1)
				cfg.PingTimeout = &timeout
				ctx := context.Background()
				cancel := func() {}
				budget := time.Second
				if parent {
					ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
					budget = 100 * time.Millisecond
				}
				defer cancel()
				done := make(chan error, 1)
				start := time.Now()
				go func() { done <- xredis.InitContext(ctx, []*xredis.Config{cfg}) }()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("client did not reach stalled stage")
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("stalled server initialized")
					}
				case <-time.After(budget + 500*time.Millisecond):
					t.Fatal("startup exceeded timeout")
				}
				if elapsed := time.Since(start); elapsed > budget+500*time.Millisecond {
					t.Fatalf("startup elapsed %v", elapsed)
				}
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("failed startup did not close connection")
				}
				if xredis.Get(cfg.Alias) != nil {
					t.Fatal("failed startup was registered")
				}
			})
		}
	}
}

func TestBatchFailureClosesEarlierClient(t *testing.T) {
	cfg, _, closed, cleanup := stubConfig("prepared-before-invalid", "")
	cfg.ExtraOptions.ReadTimeout = time.Second
	cfg.ExtraOptions.WriteTimeout = time.Second
	defer cleanup()
	huge := overflowingPoolCount(t)
	invalid := &xredis.Config{Alias: "invalid-second", Addr: "audit.invalid:6379", PoolSize: &huge}
	if err := xredis.Init([]*xredis.Config{cfg, invalid}); err == nil {
		t.Fatal("invalid batch initialized")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("earlier prepared client leaked")
	}
	if xredis.Get(cfg.Alias) != nil {
		t.Fatal("partial batch was registered")
	}
}

func TestStartupClonePreservesRuntimeOptionsAndOwnership(t *testing.T) {
	cfg, _, _, cleanup := stubConfig("runtime-options", "")
	defer cleanup()
	if err := xredis.Init([]*xredis.Config{cfg}); err != nil {
		t.Fatal(err)
	}
	defer xredis.Close(cfg.Alias)
	client := xredis.Get(cfg.Alias)
	opts := client.Options()
	if opts.ReadTimeout != -1 || opts.WriteTimeout != -1 {
		t.Fatalf("native -2 changed after startup: %v/%v", opts.ReadTimeout, opts.WriteTimeout)
	}
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("shared pool was closed: %v", err)
	}
	if cfg.ExtraOptions.ReadTimeout != -2 || cfg.ExtraOptions.WriteTimeout != -2 {
		t.Fatal("caller options mutated")
	}
}

func TestStartupDeadlineDoesNotReachRuntime(t *testing.T) {
	cfg, _, _, cleanup := stubConfig("runtime-after-startup-deadline", "")
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := xredis.InitContext(ctx, []*xredis.Config{cfg}); err != nil {
		t.Fatal(err)
	}
	defer xredis.Close(cfg.Alias)
	<-ctx.Done()
	if err := xredis.Get(cfg.Alias).Ping(context.Background()).Err(); err != nil {
		t.Fatalf("runtime inherited expired startup deadline: %v", err)
	}
}

func TestNativeHooksAndConnectionIsolation(t *testing.T) {
	for _, sentinel := range []time.Duration{-1, -2} {
		t.Run(sentinel.String(), func(t *testing.T) {
			cfg, _, _, cleanup := stubConfig("native-hooks", "")
			defer cleanup()
			cfg.ExtraOptions.ReadTimeout = sentinel
			cfg.ExtraOptions.WriteTimeout = sentinel
			var dials, connects atomic.Int32
			dial := cfg.ExtraOptions.Dialer
			cfg.ExtraOptions.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
				dials.Add(1)
				return dial(ctx, network, addr)
			}
			cfg.ExtraOptions.OnConnect = func(ctx context.Context, conn *redis.Conn) error { connects.Add(1); return conn.Ping(ctx).Err() }
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			if err := xredis.InitContext(ctx, []*xredis.Config{cfg}); err != nil {
				t.Fatal(err)
			}
			defer xredis.Close(cfg.Alias)
			if dials.Load() != 1 || connects.Load() != 1 {
				t.Fatalf("startup dial/hook count %d/%d", dials.Load(), connects.Load())
			}
			<-ctx.Done()
			client := xredis.Get(cfg.Alias)
			if err := client.Ping(context.Background()).Err(); err != nil {
				t.Fatal(err)
			}
			expected := int32(1)
			if sentinel == -2 {
				expected = 2
			}
			if dials.Load() != expected || connects.Load() != expected {
				t.Fatalf("runtime dial/hook count %d/%d want %d", dials.Load(), connects.Load(), expected)
			}
			normalized := time.Duration(0)
			if sentinel == -2 {
				normalized = -1
			}
			if client.Options().ReadTimeout != normalized || client.Options().WriteTimeout != normalized {
				t.Fatal("runtime native socket semantics changed")
			}
		})
	}
}

// overflowingPoolCount produces an out-of-range value without a 32-bit compile-time overflow.
func overflowingPoolCount(t *testing.T) int {
	t.Helper()
	if strconv.IntSize < 64 {
		t.Skip("pool overflow requires a 64-bit int")
	}
	value := int64(math.MaxInt32)
	value++
	return int(value)
}
