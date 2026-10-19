//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")
)

const (
	runKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue  = "Outloud"
	themeKey  = `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`
	mbInfo    = 0x40    // MB_ICONINFORMATION
	mbWarning = 0x30    // MB_ICONWARNING
	mbFront   = 0x50000 // MB_SETFOREGROUND | MB_TOPMOST
)

func messageBox(title, text string, flags uint32) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	windows.MessageBox(0, t, c, flags|mbFront)
}

func setClipboard(s string) error {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return err
	}
	// Another program may hold the clipboard for a moment.
	opened := false
	for range 10 {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !opened {
		return errors.New("clipboard is busy")
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	h, _, err := procGlobalAlloc.Call(0x0002, uintptr(2*len(u))) // GMEM_MOVEABLE
	if h == 0 {
		return err
	}
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return err
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&u[0])), uintptr(2*len(u)))
	procGlobalUnlock.Call(h)
	if r, _, err := procSetClipboardData.Call(13, h); r == 0 { // CF_UNICODETEXT
		procGlobalFree.Call(h)
		return err
	}
	return nil
}

// lightTaskbar reports whether the taskbar uses the light theme.
func lightTaskbar() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, themeKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("SystemUsesLightTheme")
	return err == nil && v == 1
}

func exePath() string {
	exe, _ := os.Executable()
	return exe
}

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValue)
	return err == nil && strings.EqualFold(strings.Trim(v, `"`), exePath())
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if on {
		return k.SetStringValue(runValue, `"`+exePath()+`"`)
	}
	if err := k.DeleteValue(runValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// firstInstance reports whether no other copy of the tray app is running.
// The mutex handle is deliberately kept open for the life of the process.
func firstInstance() bool {
	name, _ := windows.UTF16PtrFromString(`Local\Outloud-tray`)
	_, err := windows.CreateMutex(nil, false, name)
	return !errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

func openFile(path string) {
	verb, _ := windows.UTF16PtrFromString("open")
	file, _ := windows.UTF16PtrFromString(path)
	_ = windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

// inputBox asks for a line of text. Win32 has no ready-made prompt dialog,
// so this borrows the one in the .NET VisualBasic runtime.
func inputBox(title, prompt, def string) (string, bool) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		`Add-Type -AssemblyName Microsoft.VisualBasic; [Console]::OutputEncoding = [Text.Encoding]::UTF8; `+
			`[Microsoft.VisualBasic.Interaction]::InputBox($env:AU_PROMPT, $env:AU_TITLE, $env:AU_DEFAULT)`)
	cmd.Env = append(os.Environ(), "AU_PROMPT="+prompt, "AU_TITLE="+title, "AU_DEFAULT="+def)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.Output()
	s := strings.TrimSpace(string(out))
	return s, err == nil && s != ""
}
