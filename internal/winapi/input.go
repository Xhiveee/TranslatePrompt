//go:build windows

package winapi

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ntdll             = windows.NewLazySystemDLL("ntdll.dll")
	procLstrlenW      = kernel32.NewProc("lstrlenW")
	procRtlMoveMemory = ntdll.NewProc("RtlMoveMemory")

	procEnumClipboardFormats = user32.NewProc("EnumClipboardFormats")
	procGlobalSize           = kernel32.NewProc("GlobalSize")
)

// sendKeys synthesizes a chord: press each vk in order, release in
// reverse. Down/up are two events per key in one SendInput batch.
func sendKeys(vks ...uint16) {
	events := make([]input, 0, len(vks)*2)
	for _, vk := range vks {
		events = append(events, keyEvent(vk, 0))
	}
	for i := len(vks) - 1; i >= 0; i-- {
		events = append(events, keyEvent(vks[i], keyeventfKeyUp))
	}
	procSendInput.Call(
		uintptr(len(events)),
		uintptr(unsafe.Pointer(&events[0])),
		unsafe.Sizeof(events[0]),
	)
}

func keyEvent(vk uint16, flags uint32) input {
	return input{
		type_: inputKeyboard,
		ki:    keybdInput{wVk: vk, dwFlags: flags},
	}
}

// Ctrl+A / Ctrl+C / Ctrl+V / Ctrl+Shift+V as used by the daemon.

func SendSelectAll()     { sendKeys(vkControl, vkA) }
func SendCopy()          { sendKeys(vkControl, vkC) }
func SendPaste()         { sendKeys(vkControl, vkV) }
func SendTerminalPaste() { sendKeys(vkControl, vkShift, vkV) }

// ReleaseModifiers waits until Alt/Shift/Ctrl are physically released
// (polling GetAsyncKeyState up to maxWait). If the user still holds
// them after the timeout, synthetic KEYUP events are injected so our
// simulated keystrokes don't collide with real modifiers.
func ReleaseModifiers(maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if !modifiersHeld() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, vk := range []uint16{vkMenu, vkShift, vkControl} {
		e := keyEvent(vk, keyeventfKeyUp)
		procSendInput.Call(1, uintptr(unsafe.Pointer(&e)), unsafe.Sizeof(e))
	}
}

func modifiersHeld() bool {
	for _, vk := range []uint16{vkMenu, vkShift, vkControl} {
		if r, _, _ := procGetAsyncKeyState.Call(uintptr(vk)); r&0x8000 != 0 {
			return true
		}
	}
	return false
}

// --- Clipboard (CF_UNICODETEXT only; enough for prompt text) ---

// ClipboardSeq is the current GetClipboardSequenceNumber value.
func ClipboardSeq() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

// WaitClipboardChange polls until the sequence number differs from
// `prev` or the timeout elapses. Returns true when it changed.
func WaitClipboardChange(prev uint32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ClipboardSeq() != prev {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ClipboardSeq() != prev
}

func openClipboard() bool {
	for i := 0; i < 10; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// ReadClipboardText returns the clipboard's CF_UNICODETEXT contents,
// or "" when no text is available.
func ReadClipboardText() string {
	if r, _, _ := procIsClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
		return ""
	}
	if !openClipboard() {
		return ""
	}
	defer procCloseClipboard.Call()

	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return ""
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return ""
	}
	// Copy via RtlMoveMemory instead of converting the locked address
	// to unsafe.Pointer: the buffer stays Go-managed and go vet stays
	// clean (it flags uintptr→Pointer conversions of syscall results).
	n, _, _ := procLstrlenW.Call(p)
	buf := make([]uint16, n+1)
	if n > 0 {
		procRtlMoveMemory.Call(
			uintptr(unsafe.Pointer(&buf[0])), p, n*2)
	}
	procGlobalUnlock.Call(h)
	return windows.UTF16ToString(buf)
}

// WriteClipboardText replaces the clipboard with a UTF-16 text item.
func WriteClipboardText(text string) bool {
	utf16, err := windows.UTF16FromString(text)
	if err != nil {
		return false
	}
	size := uintptr(len(utf16) * 2)

	if !openClipboard() {
		return false
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()

	hMem, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
	if hMem == 0 {
		return false
	}
	p, _, _ := procGlobalLock.Call(hMem)
	if p == 0 {
		procGlobalFree.Call(hMem)
		return false
	}
	procRtlMoveMemory.Call(
		p, uintptr(unsafe.Pointer(&utf16[0])), size)
	procGlobalUnlock.Call(hMem)

	if r, _, _ := procSetClipboardData.Call(cfUnicodeText, hMem); r == 0 {
		procGlobalFree.Call(hMem)
		return false
	}
	// Ownership of hMem now belongs to the system — do NOT free it.
	return true
}

// ClipboardItem is one clipboard format captured as raw bytes.
type ClipboardItem struct {
	Format uint32
	Data   []byte
}

// SnapshotClipboard copies every memory-backed clipboard format so the
// original contents (text, files, images carried via HGLOBAL formats)
// can be restored after the daemon briefly holds translated text.
// Formats whose data is not an HGLOBAL (e.g. CF_BITMAP GDI handles)
// have no byte block to copy and are skipped.
func SnapshotClipboard() []ClipboardItem {
	if !openClipboard() {
		return nil
	}
	defer procCloseClipboard.Call()

	var items []ClipboardItem
	for format := uintptr(0); ; {
		format, _, _ = procEnumClipboardFormats.Call(format)
		if format == 0 {
			break
		}
		h, _, _ := procGetClipboardData.Call(format)
		if h == 0 {
			continue
		}
		size, _, _ := procGlobalSize.Call(h)
		if size == 0 {
			continue // not a global-memory block (HBITMAP etc.)
		}
		p, _, _ := procGlobalLock.Call(h)
		if p == 0 {
			continue
		}
		buf := make([]byte, size)
		procRtlMoveMemory.Call(
			uintptr(unsafe.Pointer(&buf[0])), p, size)
		procGlobalUnlock.Call(h)
		items = append(items, ClipboardItem{
			Format: uint32(format), Data: buf,
		})
	}
	return items
}

// RestoreClipboard writes a snapshot back, replacing current contents.
// An empty snapshot leaves the clipboard empty.
func RestoreClipboard(items []ClipboardItem) {
	if !openClipboard() {
		return
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()

	for _, it := range items {
		size := uintptr(len(it.Data))
		hMem, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
		if hMem == 0 {
			continue
		}
		p, _, _ := procGlobalLock.Call(hMem)
		if p == 0 {
			procGlobalFree.Call(hMem)
			continue
		}
		procRtlMoveMemory.Call(
			p, uintptr(unsafe.Pointer(&it.Data[0])), size)
		procGlobalUnlock.Call(hMem)
		if r, _, _ := procSetClipboardData.Call(
			uintptr(it.Format), hMem); r == 0 {
			procGlobalFree.Call(hMem)
		}
	}
}
