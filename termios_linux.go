package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

// Linux keeps the rate in Cflag, and leaving it 0 means B0: hang up, which
// drops DTR. With DTR low MicroPython writes the console into a ring buffer
// that overwrites itself, and a fast `get` lost megabytes (2026-10-03, Pi).
func setSpeed(t *unix.Termios) {
	t.Cflag |= unix.B115200
	t.Ispeed, t.Ospeed = unix.B115200, unix.B115200
}

// discardOutput drops bytes the board never took. Otherwise close(2) waits
// for them to drain on a hung board.
func discardOutput(fd int) { unix.IoctlSetInt(fd, unix.TCFLSH, 1) } // TCOFLUSH
