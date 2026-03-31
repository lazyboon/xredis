package xredis

import (
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Config represents the configuration for a Redis client instance.
// It supports standard connection parameters and allows fine-tuning
// through ExtraOptions for advanced go-redis features.
type Config struct {
	// Alias is a unique identifier for the Redis instance, used for retrieval via Get().
	Alias string `json:"alias"`

	// Network type, either "tcp" or "unix". Default is "tcp".
	Network string `json:"network"`

	// Addr is the host:port address of the Redis server.
	Addr string `json:"addr"`

	// Username and Password for ACL-based or legacy authentication.
	Username string `json:"username"`
	Password string `json:"password"`

	// DB is the database index to be selected after connecting to the server.
	DB int `json:"db"`

	// DialTimeout specifies the maximum amount of time (in seconds)
	// to wait for a connection to be established.
	DialTimeout *uint `json:"dial_timeout"`

	// ReadTimeout specifies the amount of time (in seconds) to wait
	// for a response from the server for a single read operation.
	ReadTimeout *uint `json:"read_timeout"`

	// WriteTimeout specifies the amount of time (in seconds) to wait
	// for a response from the server for a single write operation.
	WriteTimeout *uint `json:"write_timeout"`

	// PoolTimeout specifies the amount of time (in seconds) client waits
	// for connection if all connections are busy before returning an error.
	PoolTimeout *uint `json:"pool_timeout"`

	// PoolSize is the maximum number of socket connections in the pool.
	PoolSize *int `json:"pool_size"`

	// MinIdleConn is the minimum number of idle connections
	// which is useful to build a warm-up pool.
	MinIdleConn *int `json:"min_idle_conn"`

	// ConnMaxIdleTime is the maximum amount of time (in seconds) a connection may be idle.
	// Expired connections are closed lazily by the pool.
	ConnMaxIdleTime *uint `json:"conn_max_idle_time"`

	// ConnMaxLifetime is the maximum amount of time (in seconds) a connection may be reused.
	// Expired connections are closed lazily by the pool.
	ConnMaxLifetime *uint `json:"conn_max_lifetime"`

	// PingTimeout specifies the maximum duration (in seconds) to wait when
	// pinging the Redis server during initialization. Defaults to 10 seconds.
	PingTimeout *uint `json:"ping_timeout"`

	// ExtraOptions provides a way to inject low-level go-redis options directly.
	// Values set here take the highest priority if they overlap with Config fields.
	ExtraOptions *redis.Options `json:"-"`
}

// FillDefaults populates empty fields with sensible production-ready default values.
// It returns the pointer to Config to support method chaining.
func (c *Config) FillDefaults() *Config {
	if c.PingTimeout == nil || *c.PingTimeout == 0 {
		val := uint(10)
		c.PingTimeout = &val
	}
	return c
}

// Validate checks the essential fields of the configuration.
// It returns an error if the Alias or Addr is missing.
func (c *Config) Validate() error {
	if c.Alias == "" {
		return errors.New("xredis: alias is required")
	}
	if c.Addr == "" {
		return fmt.Errorf("xredis: addr is required for alias [%s]", c.Alias)
	}
	return nil
}
