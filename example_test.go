package xredis_test

import (
	"fmt"

	"github.com/lazyboon/xredis"
)

func ExampleLimit() {
	limit := xredis.NewPerMinuteLimit(120, 10)
	fmt.Println(limit.String())
	fmt.Println(limit.Validate())
	// Output:
	// 120 req/m (burst 10)
	// <nil>
}

func ExampleConfig_Validate() {
	// Validation is local; it does not contact Redis.
	config := &xredis.Config{Alias: "main", Addr: "localhost:6379"}
	fmt.Println(config.Validate())
	// Output: <nil>
}

func ExampleLockOptions() {
	options := xredis.NewLockOption().Apply(
		xredis.WithLockToken("job:123"),
		xredis.WithLockMetadata("worker-1"),
	)
	fmt.Println(*options.Token, *options.Metadata)
	// Output: job:123 worker-1
}
