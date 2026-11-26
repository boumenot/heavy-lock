package contract

import (
	"testing"

	heavylockv2 "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/protobuf/proto"
)

func TestCheckServiceIdentityAndVersion(t *testing.T) {
	health := &heavylockv2.Health{
		Live:            true,
		Ready:           false,
		ServiceVersion:  "0.1.0",
		ServiceIdentity: ServiceIdentity,
		ApiMajor:        APIMajor,
		ApiMinor:        APIMinor,
	}
	if err := CheckService(health, 0); err != nil {
		t.Fatalf("compatible but recovering broker must be recognizable: %v", err)
	}
	tests := []struct {
		name   string
		health *heavylockv2.Health
		minor  uint32
	}{
		{name: "missing health"},
		{name: "missing identity", health: proto.Clone(health).(*heavylockv2.Health)},
		{name: "unrelated identity", health: proto.Clone(health).(*heavylockv2.Health)},
		{name: "different major", health: proto.Clone(health).(*heavylockv2.Health)},
		{name: "older minor", health: proto.Clone(health).(*heavylockv2.Health), minor: 1},
		{name: "missing service version", health: proto.Clone(health).(*heavylockv2.Health)},
		{name: "not live", health: proto.Clone(health).(*heavylockv2.Health)},
	}
	tests[1].health.ServiceIdentity = ""
	tests[2].health.ServiceIdentity = "unrelated"
	tests[3].health.ApiMajor++
	tests[5].health.ServiceVersion = ""
	tests[6].health.Live = false
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := CheckService(tt.health, tt.minor); err == nil {
				t.Fatal("incompatible service must be rejected")
			}
		})
	}
}
