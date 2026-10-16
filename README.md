# omvcon

A console for MicroPython boards on USB serial that does what mpremote does,
minus the habits that cost replugs: no soft reset unless asked, a deadline
on every wait, fast checked file transfers, and a browser view of a
script's frames that sends clicks back. Written for the Cormorant cameras
(STM32N6, OpenMV firmware); the console commands work with any MicroPython
board, `view` with OpenMV firmware. MIT licensed.

    go install github.com/lvdlvd/omvcon@latest

    go build -o omvcon .
    ./omvcon status                 # firmware, reset cause, watchdog breadcrumbs
    ./omvcon exec 'print(1+1)'
    ./omvcon -t 60s run script.py   # stream its output; Ctrl-C stops the board's code too
    ./omvcon get /sdcard/cal/e1.bin
    ./omvcon put t1c.py /sdcard/t1c.py
    ./omvcon tail                   # watch a running main.py without touching it
    ./omvcon reset soft             # timed, then the per-stage boot times
    ./omvcon reset hard             # machine.reset(), wait for the console
    ./omvcon view script.py         # its omvview frames in a browser page, clicks back

## What is different from mpremote

- **No soft reset unless asked.** mpremote sends Ctrl-D at the start of
  every session. omvcon interrupts the running script (it has to, to get a
  prompt) and goes straight to the raw REPL.
- **Every wait has a deadline**, and a failed attach says which of four
  things it saw: no console device, a device that won't open, a silent
  board, or a board that talks but gives no prompt.
- **A silent board is followed through its watchdog reset**, on firmware
  that has one. The Cormorant OpenMV firmware (v1.2 and later) resets a
  board that hangs in C within 30 s, records the boot or soft-reset stage
  plus the PC it hung at, and records crashes (HardFault PC, LR, CFSR, HFSR,
  BFAR). omvcon waits for the reset and prints `omv.last_reset()` and
  `omv.boot_times()`. On firmware without these, `status` says so and a
  silent board ends with exit code 3 after 40 s. Look the PC up with
  `arm-none-eabi-addr2line -f -e firmware.elf 0x...`, using the ELF of the
  exact build.
- **get and put stream raw bytes** over the raw REPL, with a sha256 check
  on both ends: about 1-5 MB/s where `mpremote cp` manages ~16 KB/s.
- On macOS it holds a `caffeinate -i` for as long as it runs.

Tested on macOS and on Linux (Pi 5, `/dev/ttyACM0`). Two things differ
between them. On Linux the baud rate lives in termios `Cflag`, and leaving
it 0 hangs up the line: DTR drops, the board's console output goes into a
ring buffer that overwrites itself, and a fast `get` loses data. Linux also
returns 0 bytes, with no error, from a tty whose device has gone, so a
read that brings nothing checks that the device node still exists. macOS
holds the port ~13 s at close after a hung board left bytes unsent; Linux
doesn't.

Exit codes: 1 the code raised or a transfer didn't check out, 2 usage,
3 board unreachable.

## view: frames in a browser, clicks back to the script

    ./omvcon view ../lwir/thermal.py              # page at http://localhost:8080/
    ./omvcon view -open -http :8090 script.py     # every interface, opens the browser

The script imports `omvview` and calls `omvview.send(img)` for each frame
it wants shown and `omvview.poll()` for the page's events:

    import omvview
    while True:
        img = csi0.snapshot()
        omvview.send(img)            # capped at 15 fps; img is not changed
        ev = omvview.poll()          # None, ('click', x, y) or ('box', x, y, w, h)

No copy of `omvview.py` is needed on the card: view embeds it and
installs it before the script runs (a class in `sys.modules`, since this
MicroPython cannot construct a module object; set its options through the
arguments, `send(img, fps=..., quality=...)`, not as attributes). A copy on
the card only makes `import omvview` work elsewhere, where `ACTIVE` is
False, `send()` does nothing and `poll()` returns None. So a script that
uses it still runs unchanged from the IDE or as main.py.

The page shows the newest frame (MJPEG at `/stream`) under a canvas: a
click posts `click X Y`, a drag posts `box X Y W H`, both in the JPEG's
pixels, and a status line shows the frame rate and the last event. For
scripts and curl: `GET /frame.jpg`, `GET /status` (JSON), `POST /event`
with the line as the body. The script's text output goes to the terminal
as with `run`, and Ctrl-C stops it the same way.

Packets, on the script's stdout among its text:

| bytes | |
|---|---|
| 5 | magic `ff 4f 4d 56 46` (`b"\xffOMVF"`) |
| 4 | JPEG length n, u32 little-endian |
| 4 | crc32 of the JPEG (zlib/IEEE, `binascii.crc32`), u32 little-endian |
| n | the JPEG |

Printed text never holds 0xFF, which is how omvcon finds a packet; it
reads one by its length, so 0x04 bytes inside a JPEG do not end the raw
REPL's output, and drops one whose crc does not match. Events go to the
script's stdin as text lines; omvcon only writes lines it has parsed and
reformatted, so a 0x03 (interrupt) or 0x04 never reaches the board.
`send()` turns Ctrl-C off while it writes a packet (an interrupt mid-packet
would leave omvcon waiting for the rest); a Ctrl-C that arrives then lands
in stdin, `poll()` raises it, and omvcon repeats Ctrl-C every second until
the script stops. Events posted after the Ctrl-C are not sent: the raw REPL
would read what the script left unread as code.

Measured on D008 (Tiny1-C board, v1.3), 2026-10-16:

| | |
|---|---|
| `send()` of a busy 256x192 RGB565 test frame (~24 KB JPEG, q80) | 4.5 ms |
| the same, uncapped, through the console | 128 fps, 3.0 MB/s, 0 bad of 1790 |
| a plain 256x192 frame, uncapped | 234 fps |
| `lwir/thermal.py` while a webcam host also streams 720p | 11.8 fps, ~16 KB frames, 0 bad of 643 |

So the console carries frames like these at ~3 MB/s, the same floor as
`get`, and the 15 fps default cap uses an eighth of it. `select.poll` on
`sys.stdin` works while a script runs under the raw REPL, and a Ctrl-C at
234 fps landed mid-packet and came out of `poll()` as designed.

## Where `get`'s 4 MB/s goes (a Cormorant camera, OpenMV 5.0.1, 2026-10-12)

Measured on the board with the same 16 KB loop `get` runs, over a 540 MB
recording on a 128 GB card:

| step | rate |
|---|---|
| card read alone | 16.3 MB/s |
| read + sha256 (hashlib) | 6.0 MB/s |
| read + crc32 (binascii) | 11.5 MB/s |
| read + console write (no hash) | 3.1 MB/s |
| read + sha256 + console write (what `get` does) | 2.2 MB/s in this test, 4.3 MB/s as `get` |

So the console transmit path is the floor (TinyUSB's CDC transmit buffer
is 4 KB in the Cormorant board config (`CFG_TUD_CDC_TX_BUFSIZE`), and
MicroPython's console write polls it), and the sha256 on the board costs
almost as much again. To get near the card's rate: (1) swap sha256 for
crc32 on both ends (a change in this tool only); (2) raise
`CFG_TUD_CDC_TX_BUFSIZE` (and the console write chunking) in the firmware,
or add a bulk vendor endpoint for file transfers; (3) a card reader beats
all of it for bulk offloads. USB mass storage stays off on purpose (dual
access corrupted a card once; see the board config).

## Which parts need what

| command | needs |
|---|---|
| `exec`, `run`, `tail`, `get`, `put`, `reset soft`, `reset hard` | any MicroPython with a raw REPL on a USB serial console (`machine.reset()` for the hard reset; `hashlib` for the transfer check, else the transfer is not checked) |
| `status` | the same; adds the reset record and boot times on firmware with `omv.last_reset()` and `omv.boot_times()` |
| following a silent board through its watchdog reset | firmware with a watchdog that resets a hung board |
| `view` | OpenMV firmware: `image` with `to_jpeg`, `binascii.crc32`, `select.poll` on `sys.stdin` |

Measured on Cormorant cameras (OpenMV 5.0.1 base, MicroPython 1.28 and
1.29); not yet on a stock OpenMV board.
