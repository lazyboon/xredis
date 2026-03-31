package xredis

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	// mu protects the instances map for thread-safe access.
	mu sync.RWMutex
	// instances stores redis.Client indexed by their unique alias.
	instances = make(map[string]*redis.Client)

	errNilConfig = errors.New("xredis: config is nil")
)

type preparedInstance struct {
	alias  string
	client *redis.Client
}

// Init initializes multiple Redis instances at once.
// If any instance fails to initialize, it triggers a rollback by closing
// all instances successfully opened during this specific call to prevent resource leaks.
func Init(configs []*Config) error {
	prepared, err := prepareClients(configs)
	if err != nil {
		return err
	}
	if err = registerPrepared(prepared); err != nil {
		closePrepared(prepared)
		return err
	}
	return nil
}

func prepareClients(configs []*Config) ([]preparedInstance, error) {
	prepared := make([]preparedInstance, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))
	for _, cfg := range configs {
		client, err := buildClient(cfg)
		if err != nil {
			closePrepared(prepared)
			return nil, err
		}

		if _, dup := seen[cfg.Alias]; dup {
			_ = client.Close()
			closePrepared(prepared)
			return nil, fmt.Errorf("xredis: duplicate alias [%s] in init configs", cfg.Alias)
		}
		seen[cfg.Alias] = struct{}{}

		prepared = append(prepared, preparedInstance{
			alias:  cfg.Alias,
			client: client,
		})
	}
	return prepared, nil
}

// registerPrepared atomically registers a fully prepared client batch.
func registerPrepared(prepared []preparedInstance) error {
	mu.Lock()
	defer mu.Unlock()

	for _, item := range prepared {
		if _, exists := instances[item.alias]; exists {
			return fmt.Errorf("xredis: alias [%s] is already registered", item.alias)
		}
	}

	for _, item := range prepared {
		instances[item.alias] = item.client
	}
	return nil
}

// closePrepared best-effort closes a prepared client batch.
func closePrepared(prepared []preparedInstance) {
	for _, item := range prepared {
		_ = item.client.Close()
	}
}

// buildClient constructs and health-checks a redis client from Config.
// It does not register the client into the global instance registry.
func buildClient(cfg *Config) (*redis.Client, error) {
	if cfg == nil {
		return nil, errNilConfig
	}

	cfg.FillDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// 1. Configure merging logic
	var options redis.Options
	if cfg.ExtraOptions != nil {
		options = *cfg.ExtraOptions
	}

	// Priority: ExtraOptions (highest) > Config Fields.
	if options.Addr == "" {
		options.Addr = cfg.Addr
	}
	if options.Network == "" {
		options.Network = cfg.Network
	}
	if options.Username == "" {
		options.Username = cfg.Username
	}
	if options.Password == "" {
		options.Password = cfg.Password
	}
	// Keep strict priority for DB too: if ExtraOptions is provided,
	// we do not override DB from Config (0 can be an intentional value).
	if cfg.ExtraOptions == nil {
		options.DB = cfg.DB
	}

	// Helper functions to map optional Config pointers to time.Duration/int.
	mapDuration := func(dst *time.Duration, src *uint) {
		if *dst == 0 && src != nil {
			*dst = time.Duration(*src) * time.Second
		}
	}
	mapInt := func(dst *int, src *int) {
		if *dst == 0 && src != nil {
			*dst = *src
		}
	}

	mapDuration(&options.DialTimeout, cfg.DialTimeout)
	mapDuration(&options.ReadTimeout, cfg.ReadTimeout)
	mapDuration(&options.WriteTimeout, cfg.WriteTimeout)
	mapDuration(&options.PoolTimeout, cfg.PoolTimeout)
	mapInt(&options.PoolSize, cfg.PoolSize)
	mapInt(&options.MinIdleConns, cfg.MinIdleConn)
	mapDuration(&options.ConnMaxIdleTime, cfg.ConnMaxIdleTime)
	mapDuration(&options.ConnMaxLifetime, cfg.ConnMaxLifetime)

	// 2. Client instantiation
	client := redis.NewClient(&options)
	ready := false
	// Ensure no leaked client on any early-return path.
	defer func() {
		if !ready {
			_ = client.Close()
		}
	}()

	// 3. Health Check (Ping)
	// Executed outside the global lock to avoid blocking other concurrent registrations.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*cfg.PingTimeout)*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("xredis [%s]: unreachable: %w", cfg.Alias, err)
	}

	ready = true
	return client, nil
}

// Get retrieves a registered Redis client by its alias.
// Returns nil if the alias is not found.
func Get(alias string) *redis.Client {
	mu.RLock()
	defer mu.RUnlock()
	return instances[alias]
}

// Close gracefully shuts down a specific Redis client and removes it from the manager.
func Close(alias string) error {
	mu.Lock()
	client, ok := instances[alias]
	if ok {
		delete(instances, alias)
	}
	mu.Unlock()

	if !ok {
		return nil
	}
	return client.Close()
}

// CloseAll shuts down all registered Redis clients and clears the manager.
// Typically called during application graceful shutdown.
func CloseAll() {
	mu.Lock()
	clients := make([]*redis.Client, 0, len(instances))
	for alias, client := range instances {
		clients = append(clients, client)
		delete(instances, alias)
	}
	mu.Unlock()

	for _, client := range clients {
		_ = client.Close()
	}
}
