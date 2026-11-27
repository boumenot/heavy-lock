//go:build !darwin

package attribution

// OSVersion is not collected outside macOS; the client OS version stays unknown.
func OSVersion() string { return "" }
