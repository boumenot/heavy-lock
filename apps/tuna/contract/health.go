package contract

import (
	"errors"
	"fmt"
	"strings"

	heavylockv2 "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
)

const (
	ServiceIdentity = "heavylock.v2"
	APIMajor        = 2
	APIMinor        = 1
)

func CheckService(health *heavylockv2.Health, minimumMinor uint32) error {
	if health == nil {
		return errors.New("broker returned no health response")
	}
	if health.ServiceIdentity != ServiceIdentity {
		return errors.New("broker service identity does not match heavylock.v2")
	}
	if health.ApiMajor != APIMajor || health.ApiMinor < minimumMinor {
		return fmt.Errorf("incompatible broker API %d.%d; need %d.%d or later within major %d",
			health.ApiMajor, health.ApiMinor, APIMajor, minimumMinor, APIMajor)
	}
	if strings.TrimSpace(health.ServiceVersion) == "" {
		return errors.New("broker did not report its service version")
	}
	if !health.Live {
		return errors.New("broker is not live")
	}
	return nil
}
