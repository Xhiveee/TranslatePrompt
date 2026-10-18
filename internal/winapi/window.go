//go:build windows

// Package winapi wraps the Win32 calls the daemon needs: foreground
// window/process detection, keyboard synthesis, and clipboard access.
package winapi

import (
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId   = user32.NewProc("GetWindowThreadProcessId")
	procSetForegroundWindow        = user32.NewProc("SetForegroundWindow")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procSendInput                  = user32.NewProc("SendInput")
	procGetAsyncKeyState           = user32.NewProc("GetAsyncKeyState")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalFree                 = kernel32.NewProc("GlobalFree")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGetWindowTextW             = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW       = user32.NewProc("GetWindowTextLengthW")
)

type HWND = uintptr

const (
	processQueryLimitedInformation = 0x1000

	cfUnicodeText = 13

	gmemMoveable = 0x0002

	vkBack    = 0x08
	vkMenu    = 0x12 // Alt
	vkShift   = 0x10
	vkControl = 0x11
	vkA       = 0x41
	vkC       = 0x43
	vkV       = 0x56

	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002
	keyeventfUnicode     = 0x0004
	keyeventfScancode    = 0x0008
)

// keybdInput mirrors Win32 KEYBDINPUT (the union member of INPUT).
type keybdInput struct {
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

// input mirrors Win32 INPUT for keyboard events (largest union member).
type input struct {
	type_ uint32
	ki    keybdInput
	// padding to the full union size (MOUSEINPUT/HARDWAREINPUT are
	// smaller than the 8-byte-aligned KEYBDINPUT block + mouse fields)
	_ [8]byte
}

const inputKeyboard = 1

// ForegroundHWND returns the HWND of the current foreground window.
func ForegroundHWND() HWND {
	hwnd, _, _ := procGetForegroundWindow.Call()
	return hwnd
}

// SetForegroundWindow restores focus to a previously saved HWND.
func SetForegroundWindow(hwnd HWND) {
	procSetForegroundWindow.Call(hwnd)
}

// WindowText returns the title of a window (for logging/debug).
func WindowText(hwnd HWND) string {
	length, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if length == 0 {
		return ""
	}
	buf := make([]uint16, length+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), length+1)
	return windows.UTF16ToString(buf)
}

// ProcessExeName resolves the executable file name ("Cursor.exe") that
// owns the given window, or "" when the process can't be queried.
func ProcessExeName(hwnd HWND) string {
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return ""
	}
	hProc, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if hProc == 0 {
		return ""
	}
	defer windows.CloseHandle(windows.Handle(hProc))

	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(
		hProc, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:size]))
}
