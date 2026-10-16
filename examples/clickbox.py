# SPDX-License-Identifier: MIT
#
# Copyright (c) 2026 lvd
#
# The smallest interactive script for `omvcon view`: the camera's picture in
# the browser; a click puts a cross there and prints the pixel, a drag draws
# the box and prints its mean brightness.
#
#     omvcon view -open examples/clickbox.py
#
# Run the same file from OpenMV IDE or as main.py and it still works: without
# omvcon view, omvview.send() does nothing and poll() returns None.

import csi

try:
    import omvview          # injected by omvcon view
except ImportError:
    omvview = None          # no copy on the card: plain script

cam = csi.CSI()
cam.reset()
cam.pixformat(csi.RGB565)
# The thermal board has one size; everything else is scaled by the ISP.
cam.framesize((256, 192) if cam.cid() == 0x2121 else (320, 240))

cross = None   # (x, y) of the last click
box = None     # (x, y, w, h) of the last drag

while True:
    img = cam.snapshot()

    # Events come in image pixels of the frame you send; take all that are
    # waiting, so a burst of clicks does not lag a frame each.
    while omvview:
        ev = omvview.poll()
        if ev is None:
            break
        if ev[0] == "click":
            cross = ev[1:]
            print("click at", cross, "pixel", img.get_pixel(cross))   # a point is one (x, y) tuple in OpenMV 5
        elif ev[0] == "box":
            box = ev[1:]
            s = img.get_statistics(roi=box)
            print("box", box, "mean L %d, min %d, max %d" % (s.l_mean, s.l_min, s.l_max))   # fields in OpenMV 5

    # Draw before sending: the page shows exactly the image you send.
    if cross:
        img.draw_cross(cross, color=(255, 0, 0), size=10, thickness=2)
    if box:
        img.draw_rectangle(box, color=(0, 255, 0), thickness=2)

    if omvview:
        omvview.send(img)   # JPEG of a copy, at most 15 fps by default
