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
  records the boot or soft-reset stage plus the PC it hung at. omvcon waits
  for that and prints `omv.last_reset()`. Look the PC up with
  `arm-none-eabi-addr2line -f -e firmware.elf 0x...`, using the ELF of the
  exact build.
- **get and put stream raw bytes** over the raw REPL, with a sha256 check
  on both ends: about 1-5 MB/s where `mpremote cp` manages ~16 KB/s.
- On macOS it holds a `caffeinate -i` for as long as it runs.

Exit codes: 1 the code raised or a transfer didn't check out, 2 usage,
3 board unreachable.
