package xredis

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	// mu guards registry membership; connection setup and shutdown run outside the lock.
	mu        sync.RWMutex
	instances = make(map[string]*redis.Client)

	errNilConfig = errors.New("xredis: config is nil")
)

// preparedInstance pairs a client with its alias for registration and error reporting.
type preparedInstance struct {
	alias  string
	client *redis.Client
}

// Init initializes named clients using a background context.
// Use [InitContext] to supply cancellation or a shared startup deadline.
func Init(configs []*Config) error {
	return InitContext(context.Background(), configs)
}

// InitContext prepares and health-checks a batch of named clients before registering it.
// Failure leaves the registry unchanged and closes clients created by this call.
// ctx controls initialization only; canceling it afterward does not close registered clients.
// Call initialization during startup and do not overlap it with registry shutdown.
func InitContext(ctx context.Context, configs []*Config) error {
	if ctx == nil {
		return errors.New("xredis: context is nil")
	}
	prepared, err := prepareClients(ctx, configs)
	if err != nil {
		return err
	}
	if err = registerPrepared(prepared); err != nil {
		return errors.Join(err, closePrepared(prepared))
	}
	return nil
}

// prepareClients builds the entire batch and cleans up partial results on failure.
func prepareClients(ctx context.Context, configs []*Config) ([]preparedInstance, error) {
	if len(configs) == 0 {
		return nil, errors.New("xredis: no clients configured")
	}
	prepared := make([]preparedInstance, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))
	for _, cfg := range configs {
		if cfg == nil {
			return nil, errors.Join(errNilConfig, closePrepared(prepared))
		}
		if _, dup := seen[cfg.Alias]; dup {
			return nil, errors.Join(fmt.Errorf("xredis: duplicate alias [%s] in init configs", cfg.Alias), closePrepared(prepared))
		}
		seen[cfg.Alias] = struct{}{}

		client, err := buildClient(ctx, cfg)
		if err != nil {
			return nil, errors.Join(err, closePrepared(prepared))
		}

		prepared = append(prepared, preparedInstance{
			alias:  cfg.Alias,
			client: client,
		})
	}
	return prepared, nil
}

// registerPrepared checks existing aliases and publishes the batch under one registry lock.
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

// closePrepared attempts every close and preserves client aliases in aggregated errors.
func closePrepared(prepared []preparedInstance) error {
	var closeErrors []error
	for _, item := range prepared {
		if err := item.client.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("xredis %q: close client: %w", item.alias, err))
		}
	}
	return errors.Join(closeErrors...)
}

// buildClient resolves configuration and checks connectivity without registering the client.
func buildClient(ctx context.Context, cfg *Config) (*redis.Client, error) {
	if cfg == nil {
		return nil, errNilConfig
	}

	config := *cfg
	config.FillDefaults()

	options, err := config.options()
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(&options)
	pingTimeout := time.Duration(*config.PingTimeout) * time.Second
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	// Native -1 removes the socket timeout but still permits context deadlines; values below -1
	// skip setting deadlines altogether. Probe those settings in a separate pool so startup
	// deadlines cannot remain on runtime connections, including on non-Unix platforms.
	isolatedProbe := options.ReadTimeout < -1 || options.WriteTimeout < -1
	probe := client
	if isolatedProbe {
		probe = client.WithTimeout(pingTimeout)
	}
	if err = probe.Ping(pingCtx).Err(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("xredis %q: ping: %w", cfg.Alias, err),
			client.Close(),
		)
	}
	if isolatedProbe {
		// The timeout clone shares this pool, so close its owner before creating the runtime client.
		if err = client.Close(); err != nil {
			return nil, fmt.Errorf("xredis %q: close health-check client: %w", cfg.Alias, err)
		}
		client = redis.NewClient(&options)
	}

	return client, nil
}

// nilClient detects nil values stored in interfaces before invoking client methods.
func nilClient(client any) bool {
	if client == nil {
		return true
	}
	v := reflect.ValueOf(client)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// Get returns the registered client for alias, or nil if no client is registered.
// The client is shared; callers must coordinate shutdown and must not close it directly.
func Get(alias string) *redis.Client {
	mu.RLock()
	defer mu.RUnlock()
	return instances[alias]
}

// Close removes and closes a named client, returning nil if the alias is absent.
// Stop its consumers first; Close does not wait for application work to finish.
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
	if err := client.Close(); err != nil {
		return fmt.Errorf("xredis %q: close client: %w", alias, err)
	}
	return nil
}

// CloseAll removes all registered clients and attempts to close each one.
// Call it after all Redis consumers have stopped. Errors retain their client aliases.
func CloseAll() error {
	mu.Lock()
	clients := make([]preparedInstance, 0, len(instances))
	for alias, client := range instances {
		clients = append(clients, preparedInstance{alias: alias, client: client})
		delete(instances, alias)
	}
	mu.Unlock()

	return closePrepared(clients)
}
