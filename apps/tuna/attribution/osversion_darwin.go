package attribution

import "syscall"

// OSVersion is the macOS product version, e.g. "26.1", or "" when unavailable.
func OSVersion() string {
	version, err := syscall.Sysctl("kern.osproductversion")
	if err != nil || len(version) > 64 {
		return ""
	}
	return version
}
