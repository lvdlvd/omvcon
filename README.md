# omvcon

A console for Cormorant cameras that does what mpremote does, minus the
habits that cost us replugs.

    go build -o omvcon .
    ./omvcon status                 # firmware, reset cause, watchdog breadcrumbs
    ./omvcon exec 'print(1+1)'
    ./omvcon -t 60s run script.py   # stream its output; Ctrl-C stops the board's code too
    ./omvcon get /sdcard/cal/e1.bin
    ./omvcon put t1c.py /sdcard/t1c.py
    ./omvcon tail                   # watch a running main.py without touching it
    ./omvcon reset soft             # timed, then the per-stage boot times
    ./omvcon reset hard             # machine.reset(), wait for the console

## What is different from mpremote

- **No soft reset unless asked.** mpremote sends Ctrl-D at the start of
  every session. omvcon interrupts the running script (it has to, to get a
  prompt) and goes straight to the raw REPL.
- **Every wait has a deadline**, and a failed attach says which of four
  things it saw: no console device, a device that won't open, a silent
  board, or a board that talks but gives no prompt.
- **A silent board is followed through its watchdog reset.** Firmware from
  openmv `dc0ef9346` on resets a board that hangs in C within 30 s, and
  records the boot or soft-reset stage plus the PC it hung at. From
  `25a8a75af` on it also records crashes (HardFault PC, LR, CFSR, HFSR),
  which used to reset the board without a trace. omvcon waits for the reset
  and prints `omv.last_reset()`. Look the PC up with
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
