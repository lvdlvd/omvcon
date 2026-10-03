package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// discardOutput drops bytes the board never took. Otherwise close(2) waits
// for them to drain, ~15 s on a hung board.
func discardOutput(fd int) { unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, 2) } // FWRITE
