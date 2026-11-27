//go:build !darwin

package procid

import "syscall"

func observe(int) (int64, State, error) { return 0, Absent, ErrUnsupported }
func inspect(int) (fingerprint, error)  { return fingerprint{}, ErrUnsupported }
func bootTime() (int64, error)          { return 0, ErrUnsupported }
func kill(int, syscall.Signal) error    { return ErrUnsupported }
func table() ([]Entry, error)           { return nil, ErrUnsupported }
