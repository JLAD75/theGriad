//go:build !windows

package idle

import "time"

// SinceLastInput is a stub on non-Windows platforms. We can't detect
// idle without platform-specific code yet (X11/Wayland on Linux, IOKit
// on macOS), so we report "very idle" so workers participate by default.
func SinceLastInput() time.Duration {
	return 24 * time.Hour
}
