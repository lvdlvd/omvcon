# SPDX-License-Identifier: MIT
#
# Copyright (c) 2026 lvd
#
# omvview: show a script's frames in `omvcon view`'s browser page, and get
# the page's clicks and boxes back.
#
#     import omvview
#     while True:
#         img = csi0.snapshot()
#         omvview.send(img)          # JPEG on a scratch copy; img is untouched
#         ev = omvview.poll()        # None, ('click', x, y) or ('box', x, y, w, h)
#
# Theory of operation. The console carries both. A frame is one binary
# packet on sys.stdout.buffer: the magic b"\xffOMVF", the JPEG's length and
# its crc32 (binascii, little-endian u32 each), then the JPEG. 0xFF never
# occurs in text the script prints, which is how the host tells a packet
# from text; it checks the crc and drops a bad packet. Events come in on
# stdin as text lines, in image pixels: "click X Y", "box X Y W H".
#
# `omvcon view` injects this file before the script runs and sets ACTIVE.
# Anywhere else (OpenMV IDE, main.py at power-up, a copy on the card) ACTIVE
# stays False, send() does nothing and poll() returns None, so nothing
# binary goes to a console that no one decodes.
#
# Ctrl-C is switched off while a packet is written: an interrupt halfway
# through would leave the host waiting for JPEG bytes that never come. A
# Ctrl-C that arrives then lands in stdin instead, and poll() raises it;
# omvcon also repeats its Ctrl-C until the script stops.
#
# Configure through the arguments, not by assigning module attributes:
# under omvcon view this "module" is a copy of the names made at injection.

import binascii
import micropython
import select
import struct
import sys
import time

import image

ACTIVE = False
FPS = 15        # send() cap: more only floods the console
QUALITY = 80

_hdr = bytearray(b"\xffOMVF\0\0\0\0\0\0\0\0")
_buf = None     # scratch copy of the frame, compressed in place
_last = None    # ticks_ms of the last frame sent
_poller = None
_line = bytearray()


def send(img, fps=FPS, quality=QUALITY):
    """Send img as one JPEG packet; returns False when skipped by the cap."""
    global _buf, _last
    if not ACTIVE:
        return False
    now = time.ticks_ms()
    if _last is not None and time.ticks_diff(now, _last) < 1000 // fps:
        return False
    _last = now
    if img.format() == image.JPEG:
        j = img.bytearray()
    else:
        n = img.size()
        if _buf is None or len(_buf) != n:
            _buf = None
            _buf = bytearray(n)
        _buf[:] = img.bytearray()
        s = image.Image(img.width(), img.height(), img.format(), buffer=_buf)
        j = s.to_jpeg(quality=quality, copy=False).bytearray()
    struct.pack_into("<II", _hdr, 5, len(j), binascii.crc32(j))
    w = sys.stdout.buffer.write
    micropython.kbd_intr(-1)
    try:
        w(_hdr)
        w(j)
    finally:
        micropython.kbd_intr(3)
    return True


def poll():
    """The next event from the page, or None; never blocks."""
    global _poller
    if not ACTIVE:
        return None
    if _poller is None:
        _poller = select.poll()
        _poller.register(sys.stdin, select.POLLIN)
    while _poller.poll(0):
        c = sys.stdin.buffer.read(1)[0]
        if c == 3:
            raise KeyboardInterrupt
        if c != 10:
            if len(_line) < 64:
                _line.append(c)
            continue
        f = bytes(_line)
        _line[:] = b""
        try:
            f = f.decode().split()
            if len(f) == 3 and f[0] == "click":
                return ("click", int(f[1]), int(f[2]))
            if len(f) == 5 and f[0] == "box":
                return ("box", int(f[1]), int(f[2]), int(f[3]), int(f[4]))
        except ValueError:
            pass
        print("omvview: ignored event", f)
    return None
