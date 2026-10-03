package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

// discardOutput drops bytes the board never took. Otherwise close(2) waits
// for them to drain on a hung board.
func discardOutput(fd int) { unix.IoctlSetInt(fd, unix.TCFLSH, 1) } // TCOFLUSH
