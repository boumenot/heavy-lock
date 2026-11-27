package contract

import (
	"testing"

	heavylockv2 "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
)

func TestCheckServiceIdentityAndVersion(t *testing.T) {
	tests := []struct {
		name, client, server string
		minor                uint64
		identity             string
		live, want           bool
	}{
		{"matching dev", ServiceVersion, ServiceVersion, 2, ServiceIdentity, true, true},
		{"different dev", ServiceVersion, "2.2.0-dev.35", 0, ServiceIdentity, true, false},
		{"legacy dev", ServiceVersion, "2.0.0-dev", 0, ServiceIdentity, true, false},
		{"dev to stable", ServiceVersion, "2.2.0", 0, ServiceIdentity, true, false},
		{"stable to dev", "2.2.0", ServiceVersion, 0, ServiceIdentity, true, false},
		{"stable compatible", "2.1.0", "2.3.5", 2, ServiceIdentity, true, true},
		{"stable older minor", "2.1.0", "2.1.9", 2, ServiceIdentity, true, false},
		{"different major", "2.1.0", "3.1.0", 0, ServiceIdentity, true, false},
		{"wrong identity", ServiceVersion, ServiceVersion, 0, "other", true, false},
		{"not live", ServiceVersion, ServiceVersion, 0, ServiceIdentity, false, false},
		{"malformed server", ServiceVersion, "not-a-semver", 0, ServiceIdentity, true, false},
		{"missing server", ServiceVersion, "", 0, ServiceIdentity, true, false},
		{"malformed client", "02.0.0", "2.0.0", 0, ServiceIdentity, true, false},
		{"invalid prerelease", "2.0.0-dev.01", "2.0.0-dev.01", 0, ServiceIdentity, true, false},
		{"stable build metadata", "2.0.0+client", "2.1.0+server", 1, ServiceIdentity, true, true},
	}
	if err := CheckService(nil, ServiceVersion, 0); err == nil {
		t.Fatal("missing health was accepted")
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := &heavylockv2.Health{
				Live: tt.live, Ready: false, ServiceIdentity: tt.identity, ServiceVersion: tt.server,
			}
			err := CheckService(health, tt.client, tt.minor)
			if (err == nil) != tt.want {
				t.Fatalf("compatibility = %v, want %t", err, tt.want)
			}
		})
	}
}
