//go:build !darwin

package runner

import (
	"errors"
	"time"
)

func observeRootExit(int) (time.Time, error) {
	return time.Time{}, errors.New("root exit observation is unavailable on this platform")
}
