//go:build !darwin

package procid

func observe(int) (int64, State, error) { return 0, Absent, ErrUnsupported }
