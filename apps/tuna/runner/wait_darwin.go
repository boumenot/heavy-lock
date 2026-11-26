//go:build darwin

package runner

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func observeRootExit(pid int) (time.Time, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return time.Time{}, fmt.Errorf("create exit observer: %w", err)
	}
	defer unix.Close(kq)
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC,
		Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		return time.Time{}, fmt.Errorf("register root exit observer: %w", err)
	}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("wait for root exit: %w", err)
		}
		if n != 1 || events[0].Ident != uint64(pid) || events[0].Flags&unix.EV_ERROR != 0 ||
			events[0].Fflags&unix.NOTE_EXIT == 0 {
			return time.Time{}, errors.New("root exit observer returned an unexpected event")
		}
		return time.Now(), nil
	}
}
