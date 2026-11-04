// Package xredis provides named Redis clients, distributed leases, and rate limiting.
// Initialize shared clients at startup and stop their consumers before calling [CloseAll].
package xredis

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config defines a named standalone Redis client. Durations are expressed in seconds.
// Nil optional fields use go-redis defaults; explicit zero values retain go-redis semantics.
// PingTimeout is the exception: nil uses 10 seconds, and explicit zero is invalid.
// Configuration and referenced data must not be mutated during initialization.
type Config struct {
	// Alias identifies the client in the package registry and must be unique.
	Alias string `json:"alias"`

	// Network selects the transport. If empty, go-redis infers tcp or unix from Addr.
	Network string `json:"network"`

	// Addr is the server address, usually host:port or a Unix socket path.
	Addr string `json:"addr"`

	// Username is the Redis ACL username; an empty value uses the default user.
	Username string `json:"username"`
	// Password authenticates the Redis connection.
	Password string `json:"password"`

	// DB selects a non-negative Redis database number.
	DB int `json:"db"`

	// DialTimeout limits connection establishment.
	DialTimeout *uint `json:"dial_timeout"`

	// ReadTimeout limits a socket read operation.
	ReadTimeout *uint `json:"read_timeout"`

	// WriteTimeout limits a socket write operation.
	WriteTimeout *uint `json:"write_timeout"`

	// PoolTimeout limits the wait for an available connection.
	PoolTimeout *uint `json:"pool_timeout"`

	// PoolSize is the base pool size, not a hard connection limit.
	// Use ExtraOptions.MaxActiveConns to bound the number of allocated connections.
	PoolSize *int `json:"pool_size"`

	// MinIdleConn is the minimum number of idle connections maintained by the pool.
	MinIdleConn *int `json:"min_idle_conn"`

	// ConnMaxIdleTime limits how long a connection may remain idle.
	// Expired connections may be removed lazily.
	ConnMaxIdleTime *uint `json:"conn_max_idle_time"`

	// ConnMaxLifetime limits how long a connection may be reused.
	// Expired connections may be removed lazily.
	ConnMaxLifetime *uint `json:"conn_max_lifetime"`

	// PingTimeout limits the initialization health check; nil uses 10 seconds.
	PingTimeout *uint `json:"ping_timeout"`

	// ExtraOptions supplies native go-redis settings. Nonzero overlapping values take precedence.
	// A zero DB falls back to Config.DB; set Config.DB to 0 to select database 0.
	// ContextTimeoutEnabled is always enabled. Native ReadTimeout/WriteTimeout -2 still skip socket
	// deadlines during normal operations; the initialization health check always applies a timeout.
	// When native settings skip deadlines, startup uses a temporary pool and runtime connects on demand.
	// Referenced objects, such as TLSConfig, are not deep-copied. Keep them immutable after initialization
	// unless their underlying API explicitly permits concurrent changes.
	ExtraOptions *redis.Options `json:"-"`
}

// FillDefaults sets an unspecified PingTimeout to 10 seconds and returns the receiver.
// It leaves explicit values unchanged and returns nil for a nil receiver.
func (c *Config) FillDefaults() *Config {
	if c == nil {
		return nil
	}
	if c.PingTimeout == nil {
		val := uint(10)
		c.PingTimeout = &val
	}
	return c
}

// Validate checks the effective configuration without modifying it or opening a connection.
func (c *Config) Validate() error {
	_, err := c.options()
	return err
}

// options merges and validates effective options without modifying caller-owned configuration.
func (c *Config) options() (redis.Options, error) {
	if c == nil {
		return redis.Options{}, errNilConfig
	}
	if strings.TrimSpace(c.Alias) == "" || strings.TrimSpace(c.Alias) != c.Alias {
		return redis.Options{}, errors.New("xredis: alias is required without surrounding whitespace")
	}
	const maxSeconds = (1<<63 - 1) / int64(time.Second)
	if c.PingTimeout != nil && (*c.PingTimeout == 0 || uint64(*c.PingTimeout) > uint64(maxSeconds)) {
		return redis.Options{}, fmt.Errorf("xredis: ping timeout must be positive and fit within time.Duration for alias %q", c.Alias)
	}

	// Preserve native overrides and fill only their unset counterparts from Config.
	var options redis.Options
	if c.ExtraOptions != nil {
		options = *c.ExtraOptions
	}

	if options.Addr == "" {
		options.Addr = c.Addr
	}
	if options.Network == "" {
		options.Network = c.Network
	}
	if options.Username == "" {
		options.Username = c.Username
	}
	if options.Password == "" {
		options.Password = c.Password
	}
	// A native DB of 0 is treated as unspecified under this package's merge rules.
	if options.DB == 0 {
		options.DB = c.DB
	}

	// Convert only effective values; overridden fields cannot overflow the active configuration.
	if err := c.applySeconds("dial_timeout", &options.DialTimeout, c.DialTimeout); err != nil {
		return redis.Options{}, err
	}
	if err := c.applySeconds("read_timeout", &options.ReadTimeout, c.ReadTimeout); err != nil {
		return redis.Options{}, err
	}
	if err := c.applySeconds("write_timeout", &options.WriteTimeout, c.WriteTimeout); err != nil {
		return redis.Options{}, err
	}
	if err := c.applySeconds("pool_timeout", &options.PoolTimeout, c.PoolTimeout); err != nil {
		return redis.Options{}, err
	}
	if err := c.applySeconds("conn_max_idle_time", &options.ConnMaxIdleTime, c.ConnMaxIdleTime); err != nil {
		return redis.Options{}, err
	}
	if err := c.applySeconds("conn_max_lifetime", &options.ConnMaxLifetime, c.ConnMaxLifetime); err != nil {
		return redis.Options{}, err
	}
	if options.PoolSize == 0 && c.PoolSize != nil {
		options.PoolSize = *c.PoolSize
	}
	if options.MinIdleConns == 0 && c.MinIdleConn != nil {
		options.MinIdleConns = *c.MinIdleConn
	}

	if strings.TrimSpace(options.Addr) == "" || strings.TrimSpace(options.Addr) != options.Addr {
		return redis.Options{}, fmt.Errorf("xredis: addr is required without surrounding whitespace for alias %q", c.Alias)
	}
	if options.DB < 0 {
		return redis.Options{}, fmt.Errorf("xredis: db must not be negative for alias %q", c.Alias)
	}
	if err := c.validatePoolCount("pool size", options.PoolSize); err != nil {
		return redis.Options{}, err
	}
	if err := c.validatePoolCount("minimum idle connections", options.MinIdleConns); err != nil {
		return redis.Options{}, err
	}
	if err := c.validatePoolCount("maximum idle connections", options.MaxIdleConns); err != nil {
		return redis.Options{}, err
	}
	if err := c.validatePoolCount("maximum active connections", options.MaxActiveConns); err != nil {
		return redis.Options{}, err
	}

	// Honor context deadlines whenever native socket timeout settings permit them.
	// Cancellation without a deadline remains subject to the client's underlying I/O behavior.
	options.ContextTimeoutEnabled = true
	return options, nil
}

// validatePoolCount checks the int32 range required by go-redis before client construction.
func (c *Config) validatePoolCount(name string, value int) error {
	if value < 0 || value > math.MaxInt32 {
		return fmt.Errorf("xredis: %s must be between 0 and %d for alias %q", name, math.MaxInt32, c.Alias)
	}
	return nil
}

// applySeconds fills an unset duration after checking that the seconds-to-duration conversion fits.
func (c *Config) applySeconds(name string, target *time.Duration, seconds *uint) error {
	if *target != 0 || seconds == nil {
		return nil
	}
	const maxSeconds = (1<<63 - 1) / int64(time.Second)
	if uint64(*seconds) > uint64(maxSeconds) {
		return fmt.Errorf("xredis: %s is too large for alias %q", name, c.Alias)
	}
	*target = time.Duration(*seconds) * time.Second
	return nil
}
