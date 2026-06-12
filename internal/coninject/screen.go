package coninject

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetConsoleScreenBufferInfo  = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procReadConsoleOutputCharacterW = kernel32.NewProc("ReadConsoleOutputCharacterW")
)

type coord struct{ X, Y int16 }

type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	dwSize              coord
	dwCursorPosition    coord
	wAttributes         uint16
	srWindow            smallRect
	dwMaximumWindowSize coord
}

// packCoord encodes a COORD into the single uintptr the Win32 ABI expects when
// a COORD is passed by value.
func packCoord(x, y int16) uintptr {
	return uintptr(uint32(uint16(x)) | uint32(uint16(y))<<16)
}

// ReadScreen attaches to the target process's console and returns the text
// currently visible in its window region, one line per row (trailing spaces
// trimmed). Like Inject, this tears down the caller's own stdio via
// FreeConsole, so run it in a short-lived / detached process.
func ReadScreen(targetPID uint32) (string, error) {
	procFreeConsole.Call()

	r, _, err := procAttachConsole.Call(uintptr(targetPID))
	if r == 0 {
		return "", fmt.Errorf("AttachConsole(%d) failed: %w", targetPID, err)
	}
	defer procFreeConsole.Call()

	name, _ := windows.UTF16PtrFromString("CONOUT$")
	handle, err := windows.CreateFile(name, genericRW, shareRW, nil, openExisting, 0, 0)
	if err != nil {
		return "", fmt.Errorf("open CONOUT$: %w", err)
	}
	defer windows.CloseHandle(handle)

	var info consoleScreenBufferInfo
	r, _, err = procGetConsoleScreenBufferInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return "", fmt.Errorf("GetConsoleScreenBufferInfo failed: %w", err)
	}

	width := info.srWindow.Right - info.srWindow.Left + 1
	if width <= 0 {
		width = info.dwSize.X
	}
	if width <= 0 {
		return "", fmt.Errorf("non-positive console width %d", width)
	}

	buf := make([]uint16, width)
	var sb strings.Builder
	for y := info.srWindow.Top; y <= info.srWindow.Bottom; y++ {
		var read uint32
		r, _, _ := procReadConsoleOutputCharacterW.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(width),
			packCoord(info.srWindow.Left, y),
			uintptr(unsafe.Pointer(&read)),
		)
		if r == 0 {
			continue
		}
		line := windows.UTF16ToString(buf[:read])
		sb.WriteString(strings.TrimRight(line, " "))
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}
