package contract

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	tunapb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
)

const (
	ServiceIdentity = "tuna"
	ServiceVersion  = "3.0.0-dev.2"
)

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

type semanticVersion struct {
	major, minor uint64
	prerelease   bool
}

func parseVersion(value string) (semanticVersion, error) {
	match := semverPattern.FindStringSubmatch(value)
	if match == nil {
		return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
	}
	major, majorErr := strconv.ParseUint(match[1], 10, 64)
	minor, minorErr := strconv.ParseUint(match[2], 10, 64)
	if majorErr != nil || minorErr != nil {
		return semanticVersion{}, fmt.Errorf("semantic version component is too large: %q", value)
	}
	if match[4] != "" {
		for _, identifier := range strings.Split(match[4], ".") {
			numeric := true
			for _, c := range identifier {
				if c < '0' || c > '9' {
					numeric = false
					break
				}
			}
			if numeric && len(identifier) > 1 && identifier[0] == '0' {
				return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
			}
		}
	}
	return semanticVersion{major: major, minor: minor, prerelease: match[4] != ""}, nil
}

func CheckService(health *tunapb.Health, clientVersion string, minimumMinor uint64) error {
	if health == nil {
		return errors.New("broker returned no health response")
	}
	if health.ServiceIdentity != ServiceIdentity {
		return errors.New("broker service identity does not match tuna")
	}
	client, err := parseVersion(clientVersion)
	if err != nil {
		return fmt.Errorf("client %w", err)
	}
	server, err := parseVersion(health.ServiceVersion)
	if err != nil {
		return fmt.Errorf("broker %w", err)
	}
	if client.major != server.major || server.minor < minimumMinor ||
		(client.prerelease || server.prerelease) && clientVersion != health.ServiceVersion {
		return fmt.Errorf("incompatible broker version %q for client %q (minimum minor %d)",
			health.ServiceVersion, clientVersion, minimumMinor)
	}
	if !health.Live {
		return errors.New("broker is not live")
	}
	return nil
}
