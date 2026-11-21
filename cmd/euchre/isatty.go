package main

import (
	"os"
	"syscall"
	"unsafe"
)

// stdinIsTerminal does a real isatty check (TCGETS succeeds only on a tty).
// The ModeCharDevice heuristic alone mistakes /dev/null for a terminal,
// which would make a scripted run block forever waiting for input.
func stdinIsTerminal() bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		os.Stdin.Fd(),
		syscall.TCGETS,
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)
	return errno == 0
}

// terminalWidth returns the terminal width in columns via TIOCGWINSZ,
// trying stdout, then stdin/stderr. Returns 80 when the size cannot be
// determined (piped output, odd terminals) so the renderer still works.
func terminalWidth() int {
	type winsize struct{ Row, Col, Xpixel, Ypixel uint16 }
	for _, f := range []*os.File{os.Stdout, os.Stdin, os.Stderr} {
		var ws winsize
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			f.Fd(),
			syscall.TIOCGWINSZ,
			uintptr(unsafe.Pointer(&ws)),
		)
		if errno == 0 && ws.Col >= 20 {
			return int(ws.Col)
		}
	}
	return 80
}
