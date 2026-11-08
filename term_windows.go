//go:build windows

package main

// The dashboard's terminal on Windows: the console through kernel32, which
// every Windows program links: key presses read as console input records,
// the screen drawn with the console's own escape-code support (Windows 10
// and later).

import (
	"syscall"
	"time"
	"unsafe"
)

const (
	termEnableProcessedInput            = 0x0001
	termEnableLineInput                 = 0x0002
	termEnableEchoInput                 = 0x0004
	termEnableVirtualTerminalProcessing = 0x0004
	termKeyEvent                        = 0x0001
	termVKUp                            = 0x26
	termVKDown                          = 0x28
)

var (
	termKernel32                      = syscall.NewLazyDLL("kernel32.dll")
	termSetConsoleMode                = termKernel32.NewProc("SetConsoleMode")
	termGetNumberOfConsoleInputEvents = termKernel32.NewProc("GetNumberOfConsoleInputEvents")
	termReadConsoleInputW             = termKernel32.NewProc("ReadConsoleInputW")
	termGetConsoleScreenBufferInfo    = termKernel32.NewProc("GetConsoleScreenBufferInfo")
)

// termInputRecord is INPUT_RECORD with its KEY_EVENT_RECORD; the other
// events fit in it too.
type termInputRecord struct {
	eventType       uint16
	_               uint16
	keyDown         int32
	repeatCount     uint16
	virtualKeyCode  uint16
	virtualScanCode uint16
	unicodeChar     uint16
	controlKeyState uint32
}

// termScreenBufferInfo is CONSOLE_SCREEN_BUFFER_INFO.
type termScreenBufferInfo struct {
	size              [2]int16
	cursorPosition    [2]int16
	attributes        uint16
	window            [4]int16
	maximumWindowSize [2]int16
}

type termTTY struct {
	input, output         syscall.Handle
	saved                 bool
	inputMode, outputMode uint32
}

// termCheck is a kernel32 BOOL result as an error: FALSE is the error
// GetLastError gave. It takes Call's results whole, so a pointer argument
// stays in the Call expression, where it is kept alive and in place.
func termCheck(ok, _ uintptr, err error) error {
	if ok != 0 {
		return nil
	}
	if errno, isErrno := err.(syscall.Errno); isErrno && errno != 0 {
		return errno
	}
	return syscall.EINVAL
}

func termSetMode(console syscall.Handle, mode uint32) error {
	return termCheck(termSetConsoleMode.Call(uintptr(console), uintptr(mode)))
}

func termOpen() (*termTTY, error) {
	// An invalid handle fails in GetConsoleMode, with its own error.
	input, _ := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	output, _ := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	var inputMode, outputMode uint32
	if err := syscall.GetConsoleMode(input, &inputMode); err != nil {
		return nil, err
	}
	if err := syscall.GetConsoleMode(output, &outputMode); err != nil {
		return nil, err
	}
	raw := inputMode &^ (termEnableLineInput | termEnableEchoInput | termEnableProcessedInput)
	if err := termSetMode(input, raw); err != nil {
		return nil, err
	}
	t := &termTTY{input: input, output: output, saved: true, inputMode: inputMode, outputMode: outputMode}
	if err := termSetMode(output, outputMode|termEnableVirtualTerminalProcessing); err != nil {
		_ = t.restore()
		return nil, err
	}
	return t, nil
}

// restore puts both console modes back; it does nothing the second time.
func (t *termTTY) restore() error {
	if !t.saved {
		return nil
	}
	t.saved = false
	if err := termSetMode(t.input, t.inputMode); err != nil {
		return err
	}
	return termSetMode(t.output, t.outputMode)
}

// close does nothing: the standard handles stay open.
func (t *termTTY) close() {}

// size is the console window's columns and rows.
func (t *termTTY) size() (int, int, error) {
	var info termScreenBufferInfo
	if err := termCheck(termGetConsoleScreenBufferInfo.Call(uintptr(t.output), uintptr(unsafe.Pointer(&info)))); err != nil {
		return 0, 0, err
	}
	left, top, right, bottom := info.window[0], info.window[1], info.window[2], info.window[3]
	return int(uint16(right - left + 1)), int(uint16(bottom - top + 1)), nil
}

// keys is the keys pressed within timeout (presses only, not releases).
func (t *termTTY) keys(timeout time.Duration) ([]dashKey, error) {
	if event, _ := syscall.WaitForSingleObject(t.input, uint32(timeout.Milliseconds())); event != syscall.WAIT_OBJECT_0 {
		return nil, nil
	}
	var pending uint32
	if err := termCheck(termGetNumberOfConsoleInputEvents.Call(uintptr(t.input), uintptr(unsafe.Pointer(&pending)))); err != nil {
		return nil, err
	}
	if pending == 0 {
		return nil, nil
	}
	var records [64]termInputRecord
	var read uint32
	if err := termCheck(termReadConsoleInputW.Call(uintptr(t.input), uintptr(unsafe.Pointer(&records[0])), uintptr(len(records)), uintptr(unsafe.Pointer(&read)))); err != nil {
		return nil, err
	}
	var b []byte
	for _, r := range records[:read] {
		if r.eventType != termKeyEvent || r.keyDown == 0 {
			continue
		}
		switch r.virtualKeyCode {
		case termVKUp:
			b = append(b, "\x1b[A"...)
		case termVKDown:
			b = append(b, "\x1b[B"...)
		default:
			if r.unicodeChar <= 0xff && r.unicodeChar != 0x1b {
				b = append(b, byte(r.unicodeChar))
			}
		}
	}
	return dashParseKeys(b), nil
}
