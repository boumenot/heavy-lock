//go:build !darwin

package jobmetrics

import (
	"context"
	"errors"
)

type unsupportedSource struct{}

func DefaultSource() Source { return unsupportedSource{} }

func (unsupportedSource) Snapshot(context.Context) (Snapshot, error) {
	return Snapshot{}, errors.New("job resource sampling is not supported on this platform")
}
