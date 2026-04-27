//go:build windows

package idle

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procGetLastInputInfo = user32.NewProc("GetLastInputInfo")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetTickCount     = kernel32.NewProc("GetTickCount")
)

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32 // ms since boot, matching GetTickCount
}

// SinceLastInput returns the duration since the user's last keyboard or
// mouse input on this Windows session. Both timestamps come from the
// same GetTickCount clock, so the subtraction is correct even across
// the 32-bit wrap (~49.7 days).
func SinceLastInput() time.Duration {
	var lii lastInputInfo
	lii.cbSize = uint32(unsafe.Sizeof(lii))
	r1, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii)))
	if r1 == 0 {
		return 0
	}
	tick, _, _ := procGetTickCount.Call()
	// Use uint32 subtraction so wrap-around works as expected.
	elapsed := uint32(tick) - lii.dwTime
	return time.Duration(elapsed) * time.Millisecond
}
