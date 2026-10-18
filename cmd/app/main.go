//go:build windows

// prompt-translator: keyless Windows daemon that translates Russian
// prompts into English for AI coding assistants.
//
// Hotkeys (configurable in config.json):
//
//	Alt+Q — in-place translation in GUI apps / clipboard translate+paste
//	        in terminals (never sends Ctrl+C/Ctrl+A to a terminal)
//	Alt+W — popup overlay: type Russian, Enter translates and pastes
//	        into the previously focused window
//
// Build for release (no console window):
//
//	go build -ldflags "-H=windowsgui" -o prompt-translator.exe ./cmd/app
package main

import (
	"log"
	"runtime"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"

	"prompt-translator/internal/config"
	"prompt-translator/internal/overlay"
	"prompt-translator/internal/sanitizer"
	"prompt-translator/internal/translator"
	"prompt-translator/internal/winapi"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterHotKey     = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey   = user32.NewProc("UnregisterHotKey")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procSetTimer           = user32.NewProc("SetTimer")
	procPostThreadMessageW = user32.NewProc("PostThreadMessageW")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
)

const (
	wmHotkey  = 0x0312
	wmTimer   = 0x0113
	wmQuit    = 0x0012
	wmAppExit = 0x8001 // WM_USER+1

	modAlt      = 0x0001
	modControl  = 0x0002
	modShift    = 0x0004
	modWin      = 0x0008
	modNoRepeat = 0x4000

	hotkeyInPlace = 1
	hotkeyPopup   = 2

	timerConfigSync = 1
)

type point struct{ x, y int32 }

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type app struct {
	cfg *config.Store
	tr  *translator.Client
	pop *overlay.Popup

	uiThreadID uint32
	quitCh     chan struct{}

	registered map[int]config.Hotkey
}

func main() {
	cfgStore, err := config.Load(config.Locate())
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	a := &app{
		cfg:        cfgStore,
		tr:         translator.New(),
		pop:        &overlay.Popup{},
		quitCh:     make(chan struct{}),
		registered: map[int]config.Hotkey{},
	}
	a.pop.OnSubmit = a.onPopupSubmit

	systray.Run(a.onTrayReady, a.onTrayExit)
}

func (a *app) onTrayReady() {
	systray.SetIcon(trayIconICO())
	systray.SetTitle("Prompt Translator")
	systray.SetTooltip("RU→EN prompt translator (Alt+Q / Alt+W)")

	mReload := systray.AddMenuItem("Reload config.json", "Re-read configuration")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Exit the translator")

	go func() {
		for {
			select {
			case <-mReload.ClickedCh:
				if err := a.cfg.Reload(); err != nil {
					log.Printf("reload config: %v", err)
				}
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			case <-a.quitCh:
				return
			}
		}
	}()

	// UI thread: owns hotkey registration, the overlay window, and the
	// Win32 message pump that dispatches both.
	go a.uiLoop()
}

func (a *app) onTrayExit() {
	if a.uiThreadID != 0 {
		procPostThreadMessageW.Call(uintptr(a.uiThreadID), wmQuit, 0, 0)
	}
	close(a.quitCh)
}

// uiLoop registers hotkeys and pumps messages on a locked OS thread —
// required so WM_HOTKEY and the popup window share one pump.
func (a *app) uiLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid, _, _ := procGetCurrentThreadId.Call()
	a.uiThreadID = uint32(tid)

	a.syncHotkeys()
	// Periodic re-check so config.json edits (new hotkeys) apply
	// without restarting the daemon.
	procSetTimer.Call(0, timerConfigSync, 2000, 0)

	var msg winMsg
	for {
		ret, _, _ := procGetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if ret == 0 || int64(int32(ret)) == -1 {
			break // WM_QUIT or error
		}
		switch msg.message {
		case wmHotkey:
			a.onHotkey(int(msg.wParam))
		case wmTimer:
			if msg.wParam == timerConfigSync {
				a.syncHotkeys()
			}
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

// syncHotkeys registers/unregisters hotkeys to match current config.
func (a *app) syncHotkeys() {
	cfg := a.cfg.Get()
	want := map[int]string{
		hotkeyInPlace: cfg.Hotkeys.InPlace,
		hotkeyPopup:   cfg.Hotkeys.Popup,
	}
	for id, chord := range want {
		hk, err := config.ParseHotkey(chord)
		if err != nil {
			log.Printf("hotkey %q: %v", chord, err)
			continue
		}
		if a.registered[id] == hk {
			continue
		}
		procUnregisterHotKey.Call(0, uintptr(id))
		mods := uintptr(modNoRepeat)
		if hk.ModAlt {
			mods |= modAlt
		}
		if hk.ModCtrl {
			mods |= modControl
		}
		if hk.ModShift {
			mods |= modShift
		}
		if hk.ModWin {
			mods |= modWin
		}
		r, _, _ := procRegisterHotKey.Call(0, uintptr(id), mods, uintptr(hk.Key))
		if r == 0 {
			log.Printf("RegisterHotKey failed for %q", chord)
			continue
		}
		a.registered[id] = hk
	}
}

func (a *app) onHotkey(id int) {
	switch id {
	case hotkeyInPlace:
		a.inPlaceTranslate()
	case hotkeyPopup:
		hwnd := winapi.ForegroundHWND()
		cls := a.cfg.Get().Classify(winapi.ProcessExeName(hwnd))
		a.pop.Show(hwnd, cls == config.ClassTerminal)
	}
}

// inPlaceTranslate implements Mode A (Alt+Q).
func (a *app) inPlaceTranslate() {
	cfg := a.cfg.Get()
	hwnd := winapi.ForegroundHWND()
	cls := cfg.Classify(winapi.ProcessExeName(hwnd))
	if cls == config.ClassIgnored {
		return
	}

	// Preserve the user's clipboard so we can restore it after paste.
	saved := winapi.ReadClipboardText()

	// Don't let a still-held Alt/Q collide with synthetic keystrokes.
	winapi.ReleaseModifiers(300 * time.Millisecond)

	var source string
	switch cls {
	case config.ClassGUI:
		prev := winapi.ClipboardSeq()
		winapi.SendSelectAll()
		winapi.SendCopy()
		winapi.WaitClipboardChange(prev, time.Duration(cfg.ClipboardTimeout)*time.Millisecond)
		source = winapi.ReadClipboardText()
	case config.ClassTerminal:
		// NEVER Ctrl+A/Ctrl+C in a terminal — that injects SIGINT into
		// the CLI agent. Use whatever mouse-select already copied.
		source = saved
	}

	if source == "" || source == saved && cls == config.ClassGUI {
		// Nothing captured; restore and bail.
		winapi.WriteClipboardText(saved)
		return
	}

	translated, err := translateText(a.tr, source, cfg.SlangDictionary)
	if err != nil {
		log.Printf("translate: %v", err)
		winapi.WriteClipboardText(saved)
		return
	}

	winapi.WriteClipboardText(translated)
	if cls == config.ClassTerminal {
		winapi.SendTerminalPaste()
	} else {
		winapi.SendPaste()
	}

	time.Sleep(time.Duration(cfg.RestoreDelay) * time.Millisecond)
	winapi.WriteClipboardText(saved)
}

// onPopupSubmit implements Mode B (Alt+W): translate popup text and
// paste it into the window that was focused before the popup.
func (a *app) onPopupSubmit(text string, targetHwnd uintptr, targetIsTerminal bool) {
	if text == "" {
		return
	}
	cfg := a.cfg.Get()

	translated, err := translateText(a.tr, text, cfg.SlangDictionary)
	if err != nil {
		log.Printf("translate: %v", err)
		return
	}

	// Return focus to the app the user was typing in, then paste.
	winapi.SetForegroundWindow(targetHwnd)
	winapi.ReleaseModifiers(300 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	saved := winapi.ReadClipboardText()
	winapi.WriteClipboardText(translated)
	if targetIsTerminal {
		winapi.SendTerminalPaste()
	} else {
		winapi.SendPaste()
	}

	time.Sleep(time.Duration(cfg.RestoreDelay) * time.Millisecond)
	winapi.WriteClipboardText(saved)
}

// translateText runs the sanitizer → translator → unmask pipeline.
func translateText(tr *translator.Client, text string, slang map[string]string) (string, error) {
	prepared, originals := sanitizer.Process(text, slang)
	out, err := tr.Translate(prepared)
	if err != nil {
		return "", err
	}
	return sanitizer.Restore(out, originals), nil
}
