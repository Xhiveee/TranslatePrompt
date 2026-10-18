//go:build windows

// Package overlay implements the borderless native Win32 popup used in
// terminal mode: a WS_POPUP window hosting one multiline EDIT control.
// Enter submits, Shift+Enter inserts a newline, Escape cancels.
package overlay

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	procRegisterClassExW     = user32.NewProc("RegisterClassExW")
	procCreateWindowExW      = user32.NewProc("CreateWindowExW")
	procDefWindowProcW       = user32.NewProc("DefWindowProcW")
	procShowWindow           = user32.NewProc("ShowWindow")
	procSetFocus             = user32.NewProc("SetFocus")
	procSetWindowPos         = user32.NewProc("SetWindowPos")
	procGetSystemMetrics     = user32.NewProc("GetSystemMetrics")
	procSetWindowLongPtrW    = user32.NewProc("SetWindowLongPtrW")
	procGetWindowLongPtrW    = user32.NewProc("GetWindowLongPtrW")
	procCallWindowProcW      = user32.NewProc("CallWindowProcW")
	procGetWindowTextW       = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW = user32.NewProc("GetWindowTextLengthW")
	procSetWindowTextW       = user32.NewProc("SetWindowTextW")
	procGetKeyState          = user32.NewProc("GetKeyState")
	procSetForegroundWindow  = user32.NewProc("SetForegroundWindow")
	procCreateSolidBrush     = gdi32.NewProc("CreateSolidBrush")
	procSetTextColor         = gdi32.NewProc("SetTextColor")
	procSetBkColor           = gdi32.NewProc("SetBkColor")
)

const (
	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	wsChild        = 0x40000000
	wsExToolWindow = 0x00000080
	wsExTopMost    = 0x00000008

	swShow = 5
	swHide = 0

	hwndTopMost = ^uintptr(0) // (HWND)-1

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpShowWindow = 0x0040

	// GWLP_WNDPROC (-4) is passed as ^uintptr(3) at the call site:
	// negative indexes cannot be expressed as a uintptr constant.

	wmKeyDown      = 0x0100
	wmSize         = 0x0005
	wmCtlColorEdit = 0x0133
	wmDestroy      = 0x0002

	vkReturn = 0x0D
	vkEscape = 0x1B
	vkShift  = 0x10

	esMultiline   = 0x0004
	esAutoVScroll = 0x0040

	popupW  = 600
	popupH  = 120
	editPad = 10
)

type wndClassExW struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

const (
	colorBg   = 0x00201818 // BGR ~ RGB(24,24,32)
	colorEdit = 0x00282828
	colorText = 0x00F0F0F0
)

// Popup owns the overlay window. Create it on the thread that pumps
// the message loop (hotkey thread) and reuse it across shows.
type Popup struct {
	hwnd         uintptr
	hwndEdit     uintptr
	editOrigProc uintptr
	brushBg      uintptr
	brushEdit    uintptr

	savedHwnd     uintptr
	savedTerminal bool

	// OnSubmit runs on Enter: translated-paste workflow uses
	// savedHwnd/savedTerminal captured before the popup was shown.
	OnSubmit func(text string, targetHwnd uintptr, targetIsTerminal bool)
	OnCancel func()
}

var (
	classOnce sync.Once
	className = windows.StringToUTF16Ptr("PromptTranslatorOverlay")
	editClass = windows.StringToUTF16Ptr("EDIT")
)

func registerClass(proc uintptr) error {
	hInstance, _, _ := procGetModuleHandleW.Call(0)
	wcx := wndClassExW{
		wndProc:   proc,
		instance:  hInstance,
		className: className,
	}
	wcx.size = uint32(unsafe.Sizeof(wcx))
	r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wcx)))
	if r == 0 {
		return e
	}
	return nil
}

// wndProc dispatches to the Popup stored on the window's GWLP_USERDATA.
// The map keeps Go pointers out of the Win32 struct.
var (
	popups   = map[uintptr]*Popup{}
	popupsMu sync.Mutex
)

func popupWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	popupsMu.Lock()
	p := popups[hwnd]
	popupsMu.Unlock()

	if p != nil {
		switch msg {
		case wmSize:
			// Stretch the edit to fill the popup with a small margin.
			w := int32(lParam & 0xFFFF)
			h := int32(lParam >> 16 & 0xFFFF)
			procSetWindowPos.Call(
				p.hwndEdit, 0,
				editPad, editPad,
				uintptr(w-2*editPad), uintptr(h-2*editPad),
				0,
			)
			return 0
		case wmCtlColorEdit:
			// Dark theme for the EDIT control.
			hdc := wParam
			procSetTextColor.Call(hdc, colorText)
			procSetBkColor.Call(hdc, colorEdit)
			return p.brushEdit
		case wmDestroy:
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// editWndProc is the subclass proc for the inner EDIT control.
func editWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	popupsMu.Lock()
	var p *Popup
	for _, cand := range popups {
		if cand.hwndEdit == hwnd {
			p = cand
			break
		}
	}
	popupsMu.Unlock()
	if p == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		return r
	}

	if msg == wmKeyDown {
		switch wParam {
		case vkReturn:
			if r, _, _ := procGetKeyState.Call(vkShift); r&0x8000 != 0 {
				break // Shift+Enter → newline, fall through to default
			}
			p.submit()
			return 0
		case vkEscape:
			p.cancel()
			return 0
		}
	}
	r, _, _ := procCallWindowProcW.Call(p.editOrigProc, hwnd, msg, wParam, lParam)
	return r
}

// Show creates the popup on first use (or reshapes an existing one),
// centers it, and focuses the EDIT control. Must be called on the
// message-pump thread.
func (p *Popup) Show(savedHwnd uintptr, savedIsTerminal bool) {
	p.savedHwnd = savedHwnd
	p.savedTerminal = savedIsTerminal

	if p.brushBg == 0 {
		p.brushBg, _, _ = procCreateSolidBrush.Call(colorBg)
		p.brushEdit, _, _ = procCreateSolidBrush.Call(colorEdit)
	}

	if p.hwnd == 0 {
		classOnce.Do(func() {
			_ = registerClass(windows.NewCallback(popupWndProc))
		})

		w := uintptr(popupW)
		h := uintptr(popupH)
		sw, _, _ := procGetSystemMetrics.Call(0) // SM_CXSCREEN
		sh, _, _ := procGetSystemMetrics.Call(1) // SM_CYSCREEN
		x := (int(sw) - popupW) / 2
		y := (int(sh) - popupH) / 3

		hwnd, _, _ := procCreateWindowExW.Call(
			wsExToolWindow|wsExTopMost,
			uintptr(unsafe.Pointer(className)),
			0,
			wsPopup|wsVisible,
			uintptr(x), uintptr(y), w, h,
			0, 0, 0, 0,
		)
		if hwnd == 0 {
			return
		}
		p.hwnd = hwnd

		hwndEdit, _, _ := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(editClass)),
			0,
			wsChild|wsVisible|esMultiline|esAutoVScroll,
			editPad, editPad,
			uintptr(popupW-2*editPad), uintptr(popupH-2*editPad),
			hwnd, 0, 0, 0,
		)
		p.hwndEdit = hwndEdit
		p.editOrigProc, _, _ = procSetWindowLongPtrW.Call(
			hwndEdit, ^uintptr(3), // GWLP_WNDPROC = -4
			windows.NewCallback(editWndProc),
		)

		popupsMu.Lock()
		popups[hwnd] = p
		popupsMu.Unlock()
	} else {
		// Clear leftover text and re-show.
		procSetWindowTextW.Call(p.hwndEdit, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(""))))
		procShowWindow.Call(p.hwnd, swShow)
	}

	procSetWindowPos.Call(
		p.hwnd, hwndTopMost, 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpShowWindow,
	)
	procSetForegroundWindow.Call(p.hwnd)
	procSetFocus.Call(p.hwndEdit)
}

// IsWindow reports whether hwnd is the popup itself or its EDIT child —
// used to avoid re-capturing the popup as the paste destination when
// the popup hotkey fires while the popup is focused.
func (p *Popup) IsWindow(hwnd uintptr) bool {
	return hwnd == p.hwnd || hwnd == p.hwndEdit
}

func (p *Popup) Hide() {
	if p.hwnd != 0 {
		procShowWindow.Call(p.hwnd, swHide)
	}
}

func (p *Popup) text() string {
	if p.hwndEdit == 0 {
		return ""
	}
	n, _, _ := procGetWindowTextLengthW.Call(p.hwndEdit)
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(p.hwndEdit, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return windows.UTF16ToString(buf)
}

func (p *Popup) clear() {
	procSetWindowTextW.Call(p.hwndEdit, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(""))))
}

func (p *Popup) submit() {
	text := p.text()
	targetHwnd := p.savedHwnd
	isTerminal := p.savedTerminal
	p.clear()
	p.Hide()
	if p.OnSubmit != nil {
		p.OnSubmit(text, targetHwnd, isTerminal)
	}
}

func (p *Popup) cancel() {
	p.clear()
	p.Hide()
	if p.OnCancel != nil {
		p.OnCancel()
	}
}
