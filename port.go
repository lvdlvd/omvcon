// SPDX-License-Identifier: MIT
//
// Copyright (c) 2026 lvd

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// cdcPorts lists candidate consoles. On macOS use cu.*: tty.* blocks on carrier detect.
func cdcPorts() []string {
	var out []string
	for _, pat := range []string{"/dev/cu.usbmodem*", "/dev/ttyACM*"} {
		m, _ := filepath.Glob(pat)
		out = append(out, m...)
	}
	sort.Strings(out)
	return out
}

// port is the board's CDC console in raw mode. Reads return after at most
// 100 ms (VMIN 0, VTIME 1), so every wait below has a deadline. The fd is
// left blocking, so os.NewFile does not hand it to the runtime poller, which
// would ignore VTIME and park the read until data arrives.
type port struct {
	f    *os.File
	path string
	gone bool // the device went away under us
}

func openPort(path string) (*port, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	t.Iflag, t.Oflag, t.Lflag = 0, 0, 0
	t.Cflag = unix.CS8 | unix.CREAD | unix.CLOCAL
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 0, 1
	setSpeed(t)
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, t); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, err
	}
	active = &port{f: os.NewFile(uintptr(fd), path), path: path}
	return active, nil
}

// active is the open port, closed properly on every way out of the program.
var active *port

// Close does not wait for unsent bytes: a non-blocking close skips the
// tty's drain, and discardOutput drops the queue.
func (p *port) Close() {
	fd := int(p.f.Fd())
	discardOutput(fd)
	unix.SetNonblock(fd, true)
	p.f.Close()
	active = nil
}

// write gives up once the board has taken nothing for idle, instead of
// blocking on a board that no longer drains its OUT endpoint (pyserial's
// default write blocked forever and hung a whole SSH session on such a board).
func (p *port) write(b []byte, idle time.Duration) error {
	deadline := time.Now().Add(idle)
	fd := int(p.f.Fd())
	for len(b) > 0 {
		var w unix.FdSet
		w.Set(fd)
		tv := unix.NsecToTimeval(int64(100 * time.Millisecond))
		n, err := unix.Select(fd+1, nil, &w, nil, &tv)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return p.lost(err)
		}
		if n == 0 {
			if time.Now().After(deadline) {
				return fmt.Errorf("%w (%d bytes unsent)", errStalled, len(b))
			}
			continue
		}
		k, err := p.f.Write(b)
		if err != nil {
			return p.lost(err)
		}
		b = b[k:]
		deadline = time.Now().Add(idle)
	}
	return nil
}

func (p *port) send(s string) error { return p.write([]byte(s), 5*time.Second) }

// read returns what arrives within one VTIME period, possibly nothing.
//
// Linux returns 0 bytes at once, forever, from a tty whose device has gone,
// with no error, which is indistinguishable from an idle board except that
// the device node is gone too. So a read that brings nothing checks for it.
func (p *port) read(buf []byte) (int, error) {
	n, err := p.f.Read(buf)
	if err == io.EOF {
		err = nil
	}
	if err != nil {
		return n, p.lost(err)
	}
	if n == 0 {
		if _, err := os.Stat(p.path); err != nil {
			return 0, p.lost(unix.ENODEV)
		}
	}
	return n, nil
}

func (p *port) lost(err error) error {
	if errors.Is(err, unix.ENXIO) || errors.Is(err, unix.EIO) || errors.Is(err, unix.ENODEV) {
		p.gone = true
		return fmt.Errorf("%w (%v)", errGone, err)
	}
	return err
}

var (
	errGone    = errors.New("the board left the USB bus")
	errStalled = errors.New("the board stopped taking input")
)
