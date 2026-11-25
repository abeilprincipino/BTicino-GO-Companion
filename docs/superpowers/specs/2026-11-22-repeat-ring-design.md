# Repeated call of a street panel

## Problem

Some street panels call the same indoor unit twice for one press. On a Classe 100X
with a Sfera panel (external unit 21), every unanswered ring went like this, in 7
rings logged on 2026-11-21 and 2026-11-22:

| Time from the ring | Frame | Meaning |
| --- | --- | --- |
| 0 s | `*8*1#1#4#21*110##` + INVITE | ring |
| 2.2 s | `*8*9#1#4*21##` | identity |
| 9.7–11.8 s | `*8*3#1#3*421##` + CANCEL | the panel ends its own call (mode 3) |
| +0.3 s | `*8*1#1#4#21*110##` | the panel calls again |
| +1 s | new INVITE | |

The second call came whether the door had been opened during the first one or not. It
came only once. The panel beeps at each call. Another panel of the same installation
(external unit 213) also ends its unanswered calls with mode 3, but never calls again.
An indoor-unit hangup is `*8*3#1#4*4<indoor unit>##` (mode 4). One ring the indoor
unit hung up after 8 s was not repeated.

For the user, the house rings again after they have already opened the door, and
Home Assistant reports a second ring.

## Design

`ignore_repeat_ring` on an entrypoint (omitted while false) turns on
`app.repeatRingGuard` for the panel at that entrypoint's `devaddr`.

- The guard follows the ring call on the bus: start, answer, and the doors opened
  during it (`*8*19*<address>##`).
- On the panel's own end (mode 3) of an unanswered call of a guarded entrypoint, it
  holds the report (state and events) back for `repeatRingWindow` (4 s). It does this
  through `heldPublisher`, which now keeps one hold per owner, so the ring
  attribution's hold and this one do not end each other.
- **The panel calls again within the window, and the door was not opened.** The call
  goes on. The hold is released without the events of the end and the restart
  (`ring.cleared`, `call.incoming_ended`, `ring.started`, `call.incoming`), so to the
  clients the first call never ended. The ring holder still opens a new stream, so
  the ring clip is that of the second call.
- **The door was opened.** The guard ends the new call the way
  `POST /api/v3/call/hangup` does (bus call end, then 603), and releases the hold when
  the bus clears the ring. The events of the repeated call are dropped, and the first
  call's end is reported once. The indoor unit rings only for the moment between the
  INVITE and the decline.
- **No new call within the window.** The end is reported when the window is over,
  4 s late, and only for a guarded entrypoint.
- **The decline fails.** Everything is reported.

## Limits

- A ring of another panel within the window is taken for the repeated call, since
  the ring frame and the INVITE do not name the unit that rang.
- A neighbour opening the same door during the call counts as opening it.
- Calls the indoor unit ended are not followed. The logs show no repeated call after
  one of them; if one shows up, the guard can follow mode 4 ends too.
