// SPDX-License-Identifier: MIT
//
// Copyright (c) 2026 lvd
//
// The MicroPython raw REPL, without the soft reset that mpremote starts
// every session with.
//
// Protocol (micropython shared/runtime/pyexec.c): Ctrl-A enters the raw
// REPL and the board answers "raw REPL; CTRL-B to exit\r\n>". Code is sent
// as is and ended with Ctrl-D. The board answers "OK", runs it, and sends
// the output, \x04, the exception text if any, \x04, then ">" for the next
// one. Ctrl-D on empty input soft-resets the board, and Ctrl-B goes back to
// the friendly REPL.
//
// Large writes need no pacing: the CDC OUT endpoint NAKs while the board's
// receive FIFO is full, so nothing is lost.

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"
)

const rawBanner = "raw REPL; CTRL-B to exit\r\n>"

type board struct {
	p    *port
	in   []byte           // read from the port but not consumed yet
	intr <-chan os.Signal // the user's Ctrl-C: stop the board's code, then leave
}

// stuckError says why there is no prompt. silent: not a byte came back.
type stuckError struct {
	what   string
	silent bool
}

func (e *stuckError) Error() string { return e.what }

// fill reads once more from the port, waiting at most one VTIME period.
func (b *board) fill() (int, error) {
	buf := make([]byte, 4096)
	n, err := b.p.read(buf)
	b.in = append(b.in, buf[:n]...)
	return n, err
}

// until consumes input up to and including want. On a timeout it returns
// what arrived, consumed, and ok false.
func (b *board) until(want string, timeout time.Duration) (string, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		if i := bytes.Index(b.in, []byte(want)); i >= 0 {
			out := string(b.in[:i+len(want)])
			b.in = b.in[i+len(want):]
			return out, true, nil
		}
		if time.Now().After(deadline) {
			out := string(b.in)
			b.in = nil
			return out, false, nil
		}
		if _, err := b.fill(); err != nil {
			out := string(b.in)
			b.in = nil
			return out, false, err
		}
	}
}

// next consumes exactly n bytes, failing once idle passes with no progress.
func (b *board) next(n int, idle time.Duration) ([]byte, error) {
	last := time.Now()
	for len(b.in) < n {
		k, err := b.fill()
		if err != nil {
			return nil, err
		}
		if k > 0 {
			last = time.Now()
		} else if time.Since(last) > idle {
			return nil, fmt.Errorf("the board went silent after %d of %d bytes", len(b.in), n)
		}
	}
	out := b.in[:n:n]
	b.in = b.in[n:]
	return out, nil
}

// enterRaw stops whatever the board runs and switches to the raw REPL.
// There is no way to a prompt past a running script without a Ctrl-C.
func (b *board) enterRaw() error {
	if err := b.p.send("\r\x03\x03"); err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	if err := b.p.send("\x01"); err != nil {
		return err
	}
	out, ok, err := b.until(rawBanner, 3*time.Second)
	if err != nil || ok {
		return err
	}
	if out == "" {
		return &stuckError{"the console is silent: the board is enumerated but answers nothing", true}
	}
	return &stuckError{fmt.Sprintf("the board talks but gives no raw prompt; last output: %q", tail(out, 200)), false}
}

func (b *board) leaveRaw() { b.p.send("\x02") }

// start sends code to run and waits for the board to accept it.
func (b *board) start(code string) error {
	if err := b.p.write([]byte(code+"\x04"), 10*time.Second); err != nil {
		return err
	}
	out, ok, err := b.until("OK", 5*time.Second)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the board did not accept the code: %q", tail(out, 200))
	}
	return nil
}

// exec runs code and streams its output to w; see finish.
func (b *board) exec(code string, w io.Writer, timeout time.Duration) (exc string, err error) {
	if err := b.start(code); err != nil {
		return "", err
	}
	return b.finish(w, timeout)
}

// finish streams the rest of a reply: output up to \x04, the exception up to
// \x04, then the prompt. A non-empty exc is the exception the code raised.
// timeout 0 means none. When it passes, or the user hits Ctrl-C, the board's
// code gets a Ctrl-C too, and we still read up to the prompt so the board
// isn't left halfway through its output.
func (b *board) finish(w io.Writer, timeout time.Duration) (exc string, err error) {
	var ex bytes.Buffer
	section := 0 // 0 output, 1 exception, 2 waiting for '>'
	start := time.Now()
	var stopAt time.Time
	for {
		for len(b.in) > 0 {
			c := b.in[0]
			b.in = b.in[1:]
			switch {
			case section == 2:
				if c == '>' {
					return ex.String(), nil
				}
			case c == 0x04:
				section++
			case section == 0:
				w.Write([]byte{c})
			default:
				ex.WriteByte(c)
			}
		}

		if stopAt.IsZero() {
			select {
			case <-b.intr:
				stopAt = time.Now()
			default:
				if timeout > 0 && time.Since(start) > timeout {
					fmt.Fprintf(os.Stderr, "omvcon: %v passed, stopping the board's code\n", timeout)
					stopAt = time.Now()
				}
			}
			if !stopAt.IsZero() {
				b.p.send("\x03")
			}
		} else if time.Since(stopAt) > 5*time.Second {
			return ex.String(), fmt.Errorf("the board did not stop within 5 s of a Ctrl-C")
		}

		if _, err := b.fill(); err != nil {
			return ex.String(), err
		}
	}
}

// softReset sends Ctrl-D in the raw REPL and waits for the raw prompt to
// come back. That covers the soft-reset teardown, the re-init and _boot.py.
func (b *board) softReset() (time.Duration, error) {
	start := time.Now()
	if err := b.p.send("\x04"); err != nil {
		return 0, err
	}
	out, ok, err := b.until(rawBanner, 45*time.Second)
	d := time.Since(start)
	if err != nil {
		return d, err
	}
	if !ok {
		return d, &stuckError{fmt.Sprintf("the soft reset did not finish in %.0f s; last output: %q", d.Seconds(), tail(out, 200)), out == ""}
	}
	return d, nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
