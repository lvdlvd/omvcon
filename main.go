// SPDX-License-Identifier: MIT
//
// Copyright (c) 2026 lvd

// omvcon talks to a Cormorant camera's MicroPython console. It is mpremote
// with three things changed.
//
//   - It never soft-resets the board on its own. mpremote starts every
//     session with Ctrl-D. That reruns the soft-reset teardown, and on board
//     #7 that teardown was the prime suspect for the console "wedges". It
//     also throws away whatever state the last session left. Here a soft
//     reset happens only when asked for (reset soft), and it is timed.
//   - Every wait has a deadline. When there is no prompt, it says what it
//     saw: no console, a console that will not open, a silent board, or a
//     board that talks but gives no prompt. A silent board on the watchdog
//     firmware (openmv dc0ef9346) resets itself within 30 s. omvcon waits
//     for that, then reports where the board was stuck (omv.last_reset()).
//   - get and put move files as raw bytes over the raw REPL, checked with
//     sha256: about 1-5 MB/s where `mpremote cp` manages ~16 KB/s.
//
// It still has to Ctrl-C whatever the board is running to reach a prompt.
// Use tail to watch a running script without disturbing it.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const usage = `usage: omvcon [-p port] [-t timeout] command [args]

  status                 stop what runs; report firmware, reset cause, watchdog
                         breadcrumbs and boot times
  exec CODE              run CODE in the raw REPL, stream its output
  run FILE               run a local Python file the same way
  get REMOTE [LOCAL]     copy a file off the board (LOCAL defaults to its base name)
  put LOCAL REMOTE       copy a file onto the board
  tail                   print what the board prints; sends nothing
  reset soft|hard        soft: Ctrl-D, timed, then boot times
                         hard: machine.reset(), wait for the console to come back
  view [-http ADDR] [-open] FILE
                         run FILE with omvview: its frames in a browser page
                         (default localhost:8080), clicks and boxes sent back

-t bounds exec, run, view and tail (0: no limit). On a timeout or Ctrl-C the board's
code is interrupted and omvcon reads up to the next prompt before it exits.
`

var (
	fPort    = flag.String("p", "", "console device (default: the only /dev/cu.usbmodem* or /dev/ttyACM*)")
	fTimeout = flag.Duration("t", 0, "limit for exec, run and tail")
)

const (
	exitPython      = 1 // the code raised, or a transfer did not check out
	exitUsage       = 2
	exitUnreachable = 3
)

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(exitUsage)
	}

	path, err := pickPort()
	if err != nil {
		fail(exitUnreachable, err)
	}

	// A sleeping Mac stops reading the console mid-session.
	if runtime.GOOS == "darwin" {
		exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid())).Start()
	}

	intr := make(chan os.Signal, 1)
	signal.Notify(intr, os.Interrupt)

	cmd, args := args[0], args[1:]
	switch {
	case cmd == "tail" && len(args) == 0:
		tailCmd(path, intr)
	case cmd == "status" && len(args) == 0:
		b := attach(path, intr)
		if !watchdogReported {
			status(b)
		}
		b.leaveRaw()
	case cmd == "exec" && len(args) == 1:
		run(attach(path, intr), args[0])
	case cmd == "run" && len(args) == 1:
		code, err := os.ReadFile(args[0])
		if err != nil {
			fail(exitUsage, err)
		}
		run(attach(path, intr), string(code))
	case cmd == "view" && len(args) >= 1:
		viewCmd(path, intr, args)
	case cmd == "get" && (len(args) == 1 || len(args) == 2):
		local := args[0][strings.LastIndex(args[0], "/")+1:]
		if len(args) == 2 {
			local = args[1]
		}
		get(attach(path, intr), args[0], local)
	case cmd == "put" && len(args) == 2:
		put(attach(path, intr), args[0], args[1])
	case cmd == "reset" && len(args) == 1 && args[0] == "soft":
		resetSoft(attach(path, intr), path)
	case cmd == "reset" && len(args) == 1 && args[0] == "hard":
		resetHard(attach(path, intr), path)
	default:
		flag.Usage()
		os.Exit(exitUsage)
	}
}

func fail(code int, err error) {
	fmt.Fprintln(os.Stderr, "omvcon:", err)
	exit(code)
}

func exit(code int) {
	if active != nil {
		if code == exitUnreachable && runtime.GOOS == "darwin" {
			// Measured 2026-10-03: ~13 s, in the kernel, even on exit without close.
			fmt.Fprintln(os.Stderr, "omvcon: macOS holds the port ~13 s while it gives up on bytes the board never took")
		}
		active.Close()
	}
	os.Exit(code)
}

// unresponsive says the board is enumerated but not running its console:
// hung in C, or the watchdog is resetting it right now.
func unresponsive(err error) bool {
	var stuck *stuckError
	return errors.Is(err, errGone) || errors.Is(err, errStalled) || (errors.As(err, &stuck) && stuck.silent)
}

func pickPort() (string, error) {
	if *fPort != "" {
		if _, err := os.Stat(*fPort); err != nil {
			return "", fmt.Errorf("no console at %s: the board is not on the USB bus", *fPort)
		}
		return *fPort, nil
	}
	ports := cdcPorts()
	switch len(ports) {
	case 0:
		return "", errors.New("no console found (/dev/cu.usbmodem*, /dev/ttyACM*): the board is not on the USB bus")
	case 1:
		return ports[0], nil
	}
	return "", fmt.Errorf("several consoles, pick one with -p: %s", strings.Join(ports, " "))
}

// attach opens the console and gets to the raw REPL. A silent board gets one
// chance to be reset by its watchdog; anything else that goes wrong ends
// the program with a one-line diagnosis.
func attach(path string, intr <-chan os.Signal) *board {
	b, err := tryAttach(path, intr)
	if err == nil {
		return b
	}
	if !unresponsive(err) {
		fail(exitUnreachable, err)
	}
	if active != nil {
		active.Close()
	}
	return awaitWatchdog(path, intr, err)
}

// awaitWatchdog follows a silent board through its watchdog reset and shows
// what the reset left behind. It returns the board at a raw prompt.
func awaitWatchdog(path string, intr <-chan os.Signal, why error) *board {
	fmt.Fprintf(os.Stderr, "omvcon: %v\nomvcon: waiting up to 40 s for its watchdog to reset it\n", why)
	if !waitGone(path, 40*time.Second) {
		fail(exitUnreachable, errors.New("the board did not reset itself: firmware without the watchdog, or a stuck USB stack on a running CPU. Replug it."))
	}
	if !waitBack(path, 30*time.Second) {
		fail(exitUnreachable, errors.New("the board reset but its console did not come back within 30 s"))
	}
	b, err := tryAttach(path, intr)
	if err != nil {
		fail(exitUnreachable, err)
	}
	fmt.Fprintln(os.Stderr, "omvcon: the watchdog reset the board; what it left behind:")
	status(b)
	watchdogReported = true
	return b
}

var watchdogReported bool

func tryAttach(path string, intr <-chan os.Signal) (*board, error) {
	p, err := openPort(path)
	if err != nil {
		return nil, fmt.Errorf("the console %s is there but will not open (another program holding it?): %v", path, err)
	}
	b := &board{p: p, intr: intr}
	return b, b.enterRaw()
}

func waitGone(path string, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(path); err != nil {
			return true
		}
	}
	return false
}

func waitBack(path string, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			time.Sleep(500 * time.Millisecond) // let the open race settle
			return true
		}
	}
	return false
}

const statusCode = `import os, machine
print(os.uname().version)
print('reset_cause', machine.reset_cause())
import omv
if hasattr(omv, 'last_reset'):
    print('last_reset', omv.last_reset())
    print('boot_times', omv.boot_times())
else:
    print('last_reset: firmware without the watchdog')
`

func status(b *board) {
	exc, err := b.exec(statusCode, os.Stdout, 10*time.Second)
	if err != nil {
		fail(exitUnreachable, err)
	}
	if exc != "" {
		fmt.Fprint(os.Stderr, exc)
	}
}

func run(b *board, code string) {
	exc, err := b.exec(code, os.Stdout, *fTimeout)
	if err != nil {
		fail(exitUnreachable, err)
	}
	b.leaveRaw()
	if exc != "" {
		fmt.Fprint(os.Stderr, exc)
		exit(exitPython)
	}
}

func tailCmd(path string, intr <-chan os.Signal) {
	p, err := openPort(path)
	if err != nil {
		fail(exitUnreachable, err)
	}
	buf := make([]byte, 4096)
	start := time.Now()
	for *fTimeout == 0 || time.Since(start) < *fTimeout {
		select {
		case <-intr:
			return
		default:
		}
		n, err := p.read(buf)
		os.Stdout.Write(buf[:n])
		if err != nil {
			fail(exitUnreachable, err)
		}
	}
}

// line consumes one printed line from the board, without its \r\n.
func line(b *board) (string, error) {
	s, ok, err := b.until("\r\n", 10*time.Second)
	if err != nil {
		return "", err
	}
	if !ok || strings.Contains(s, "\x04") {
		// The code raised before printing: let finish show why.
		b.in = append([]byte(s), b.in...)
		exc, _ := b.finish(os.Stdout, 5*time.Second)
		return "", fmt.Errorf("the board raised: %s", strings.TrimSpace(exc))
	}
	return strings.TrimSuffix(s, "\r\n"), nil
}

const getCode = `import os, sys, binascii
try:
    import hashlib; h = hashlib.sha256()
except ImportError:
    h = None
try:
    f = open(%[1]s, 'rb'); n = os.stat(%[1]s)[6]
except OSError as e:
    f = None; print('ERR', e)
if f:
    print(n)
    b = bytearray(16384); m = memoryview(b); w = sys.stdout.buffer.write
    while True:
        k = f.readinto(b)
        if not k:
            break
        w(m[:k])
        if h:
            h.update(m[:k])
    f.close()
    print(binascii.hexlify(h.digest()).decode() if h else '')
`

func get(b *board, remote, local string) {
	start := time.Now()
	if err := b.start(fmt.Sprintf(getCode, strconv.Quote(remote))); err != nil {
		fail(exitUnreachable, err)
	}
	l, err := line(b)
	if err != nil {
		fail(exitPython, err)
	}
	if strings.HasPrefix(l, "ERR") {
		b.finish(os.Stdout, 5*time.Second)
		fail(exitPython, fmt.Errorf("%s: %s", remote, l))
	}
	n, err := strconv.Atoi(l)
	if err != nil {
		fail(exitUnreachable, fmt.Errorf("expected a file size, got %q", l))
	}
	data, err := b.next(n, 5*time.Second)
	if err != nil {
		fail(exitUnreachable, err)
	}
	sum, err := line(b)
	if err != nil {
		fail(exitUnreachable, err)
	}
	if _, err := b.finish(os.Stdout, 5*time.Second); err != nil {
		fail(exitUnreachable, err)
	}
	b.leaveRaw()
	check(data, sum)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		fail(exitUsage, err)
	}
	d := time.Since(start)
	fmt.Fprintf(os.Stderr, "%s -> %s: %d bytes in %.1f s (%.0f KB/s)\n", remote, local, n, d.Seconds(), float64(n)/1024/d.Seconds())
}

// The board's Ctrl-C handling is switched off while the data flows, or a
// 0x03 byte in the file would interrupt the copy.
const putCode = `import sys, micropython, binascii
try:
    import hashlib; h = hashlib.sha256()
except ImportError:
    h = None
try:
    f = open(%s, 'wb')
except OSError as e:
    f = None; print('ERR', e)
if f:
    micropython.kbd_intr(-1)
    n = %d
    print('READY')
    r = sys.stdin.buffer.read
    while n:
        d = r(min(n, 4096)); f.write(d); n -= len(d)
        if h:
            h.update(d)
    f.close()
    micropython.kbd_intr(3)
    print(binascii.hexlify(h.digest()).decode() if h else '')
`

func put(b *board, local, remote string) {
	data, err := os.ReadFile(local)
	if err != nil {
		fail(exitUsage, err)
	}
	start := time.Now()
	if err := b.start(fmt.Sprintf(putCode, strconv.Quote(remote), len(data))); err != nil {
		fail(exitUnreachable, err)
	}
	l, err := line(b)
	if err != nil {
		fail(exitPython, err)
	}
	if l != "READY" {
		b.finish(os.Stdout, 5*time.Second)
		fail(exitPython, fmt.Errorf("%s: %s", remote, l))
	}
	if err := b.p.write(data, 10*time.Second); err != nil {
		fail(exitUnreachable, err)
	}
	sum, err := line(b)
	if err != nil {
		fail(exitUnreachable, err)
	}
	if _, err := b.finish(os.Stdout, 5*time.Second); err != nil {
		fail(exitUnreachable, err)
	}
	b.leaveRaw()
	check(data, sum)
	d := time.Since(start)
	fmt.Fprintf(os.Stderr, "%s -> %s: %d bytes in %.1f s (%.0f KB/s)\n", local, remote, len(data), d.Seconds(), float64(len(data))/1024/d.Seconds())
}

// check compares the board's sha256 with ours. An empty sum means the
// firmware has no hashlib, so there is nothing to compare.
func check(data []byte, sum string) {
	if sum == "" {
		fmt.Fprintln(os.Stderr, "omvcon: no hashlib on the board, transfer not checked")
		return
	}
	h := sha256.Sum256(data)
	if !bytes.Equal([]byte(hex.EncodeToString(h[:])), []byte(sum)) {
		fail(exitPython, fmt.Errorf("sha256 mismatch: board %s, host %s", sum, hex.EncodeToString(h[:])))
	}
}

func resetSoft(b *board, path string) {
	d, err := b.softReset()
	if unresponsive(err) {
		b.p.Close()
		b = awaitWatchdog(path, b.intr, err)
		b.leaveRaw()
		exit(exitUnreachable)
	}
	if err != nil {
		fail(exitUnreachable, err)
	}
	fmt.Fprintf(os.Stderr, "soft reset: raw prompt back after %.1f s\n", d.Seconds())
	status(b)
	b.leaveRaw()
}

// resetHard leaves the board running its main.py: nothing attaches afterwards.
func resetHard(b *board, path string) {
	start := time.Now()
	b.p.write([]byte("import machine\nmachine.reset()\x04"), 5*time.Second)
	b.p.Close()
	if !waitGone(path, 5*time.Second) {
		fail(exitUnreachable, errors.New("the console did not go away: the board did not reset"))
	}
	gone := time.Since(start)
	if !waitBack(path, 45*time.Second) {
		fail(exitUnreachable, errors.New("the board reset but its console did not come back within 45 s"))
	}
	fmt.Fprintf(os.Stderr, "hard reset: console gone after %.1f s, back after %.1f s\n", gone.Seconds(), time.Since(start).Seconds())
}
