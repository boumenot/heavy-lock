//go:build tuna_test_broker

package main

import (
	"context"
	"errors"
	"os"

	"github.com/boumenot/heavy-lock/apps/tuna/service"
)

// A tuna_test_broker build serves integration tests outside this package,
// such as the monitor's. It never takes the account's singleton lock.
func init() {
	serviceRunner = func(ctx context.Context, o service.Options) error {
		o.LockDir = os.Getenv("TUNA_TEST_LOCK_DIR")
		if o.LockDir == "" {
			return errors.New("test broker: TUNA_TEST_LOCK_DIR is not set")
		}
		return service.Run(ctx, o)
	}
}
