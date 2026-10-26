// Package devmode holds the test-only switch that runs the agent on a
// machine without GPU passthrough (a cloud VM with nested KVM), so the
// control-plane path can be tested end to end before bare metal is at hand.
// It exists only in binaries built with -tags devnopassthrough, and only
// against a preview or loopback API.
package devmode

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// NoPassthrough reports whether the switch is on (tagged build and
// RUNCRATE_HOST_DEV_NO_PASSTHROUGH=1).
func NoPassthrough() bool {
	return built && os.Getenv("RUNCRATE_HOST_DEV_NO_PASSTHROUGH") == "1"
}

// Check refuses the switch against anything but a preview or loopback API.
func Check(apiURL string) error {
	if !NoPassthrough() {
		return nil
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return err
	}
	h := u.Hostname()
	if (u.Scheme == "https" && strings.HasSuffix(h, ".vercel.app")) || h == "127.0.0.1" || h == "localhost" {
		return nil
	}
	return fmt.Errorf("RUNCRATE_HOST_DEV_NO_PASSTHROUGH is only allowed against a preview or loopback API, not %s", h)
}
