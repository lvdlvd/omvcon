package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// The board ignores the rate; it only has to be a real one.
func setSpeed(t *unix.Termios) { t.Ispeed, t.Ospeed = unix.B115200, unix.B115200 }

// discardOutput drops bytes the board never took. Otherwise close(2) waits
// for them to drain, ~15 s on a hung board.
func discardOutput(fd int) { unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, 2) } // FWRITE
