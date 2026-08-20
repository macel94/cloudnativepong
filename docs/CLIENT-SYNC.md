# Client presentation and server synchronization

Cloud Native Pong uses a server-authoritative simulation with a deliberately
independent browser presentation loop. This keeps the game fair while making
paddle input feel immediate on `https://pong.belacca.com/`.

## Responsibilities

### Server

The room engine is the source of truth for:

- both paddle positions;
- ball position, velocity, wall/paddle collisions, and scoring;
- ready, playing, and finished status; and
- the winner.

The engine advances on its fixed `game.TickDuration` (currently 16 ms, about
60 Hz). Authoritative physics stays at this rate so collisions and input timing
do not depend on network delivery. The spanning browser snapshot stream runs
just below the sim rate (`StateBroadcastInterval`, currently 20 ms/about 50 Hz)
so the display loop fills in only a short gap between snapshots. Fresher
snapshots cut the perceived multiplayer lag: the local AI path renders the
per-frame sim directly, and the network path converges on that cadence instead
of stalling on a coarser broadcast. It accepts paddle **intent** (`up`/`down`), never a client-supplied
position. A client cannot move a paddle, ball, or score by sending a fabricated
coordinate.

The browser-facing room connection sends authoritative snapshots and accepts
input over both WebSocket and the optional WebTransport stream. Both
transports use the same JSON game protocol. A spectator connects with the
`spectator=1` query parameter and receives a `{"type":"spectator"}` acknowledgement
followed by the same snapshots. Spectators do not occupy either player slot,
do not send input, and do not affect room start or cleanup lifecycle.

## Input acknowledgement protocol

Each browser input contains a monotonically increasing `sequence`:

```json
{"player":1,"up":true,"down":false,"sequence":42}
```

Every authoritative snapshot echoes the newest sequence applied by the engine
for each player:

```json
{
  "type":"state",
  "state": {
    "p1_input_sequence": 42,
    "p2_input_sequence": 37
  }
}
```

The sequence is only an acknowledgement marker. It is not trusted as a
position, timestamp, or tick number. The server ignores an older sequence and
ignores a legacy sequence-less input after sequenced input has been accepted.
This preserves compatibility with old clients without allowing delayed packets
to roll the input state backward.

## Browser presentation

The browser does not render directly from the arrival of a server snapshot.
`requestAnimationFrame` owns drawing and continues at the display's cadence.
Player input intent is sent at about 30 Hz; the server holds the newest intent
and applies it on each 60 Hz simulation tick.

- **Local paddle:** client-side predicted. The current held input is applied
  immediately on animation frames, independently of state delivery. When a
  snapshot acknowledges input, the browser uses the authoritative paddle as a
  reconciliation baseline, forecasts the short presentation delay, and eases
  any correction over a bounded window. The server remains authoritative; this
  is only a visual prediction.
- **Other paddle:** latest-snapshot dead reckoning. The browser estimates the
  opponent's recent vertical velocity and advances the newest authoritative
  position on each display frame. A bounded correction decays when the next
  snapshot disagrees; the opponent is no longer intentionally rendered in the
  past.
- **Ball:** latest-snapshot dead reckoning with prediction. The authoritative
  ball velocity advances the ball between packets, but wall **and paddle**
  bounces are predicted locally (mirroring the server's rebound rules) so a
  paddle hit does not force the display into a large correction. Collisions,
  scoring, and resets remain server-controlled; a new snapshot corrects any
  residual visual error.
- **Score and game status:** updated from the newest authoritative snapshot,
  not from predicted presentation state.

The result is that all visible entities continue moving on the display frame
clock, while authoritative snapshots periodically correct the presentation.
Online extrapolation is capped at 100 ms, so a delayed connection cannot let a
display-only ball run indefinitely beyond an authoritative collision. This
removes the deliberate tick/network delay without changing the authoritative
outcome.

## Reconciliation and boundaries

Prediction is intentionally conservative:

- paddle positions are clamped to the same normalized bounds as the server;
- pending input acknowledgements are bounded in memory;
- correction is eased rather than allowed to accumulate indefinitely;
- online extrapolation is capped at 100 ms;
- the latest server snapshot can always correct a client prediction; and
- prediction stops affecting authoritative state when the connection closes or
  the server reports a finished game.

This is not a rollback or client-authoritative physics system. Online
extrapolation is display-only, bounded in time and position, and clients never
decide whether a paddle collision or point happened.

## Diagnostics

The realtime path has two network hops in production (room pod stream -> API
proxy -> Caddy -> browser), so lag is diagnosed at each hop instead of guessed
at. Every hop records the same three numbers: frame cadence (Hz), inter-frame
jitter (gap avg/p95/max), and the hop's own work inside that gap (write or
relay time).

The room snapshot stream is paced from a fixed cadence anchor rather than a
naive ticker: if a slow consumer (write backpressure) or a scheduler stall
makes a broadcast late, it re-anchors to the next interval instead of firing
a catch-up burst. This keeps delivery steady and limits drift.

Backend:

- The room pod's `streamRoomStates` logs `[diag] room_state_stream summary`
  every two seconds with broadcast Hz, gap statistics, and write cost; it also
  records `pong_room_stream_frame_gap_ms` / `pong_room_stream_write_ms`
  duration series and a `pong_room_stream_frame_over_25ms` counter.
- The API proxy relay logs `[diag] proxy_relay summary` with arrival jitter
  (room pod -> proxy cadence) and relay copy cost (proxy -> browser) and
  records `pong_proxy_frame_gap_ms` / `pong_proxy_relay_ms` series.
- `PONG_DIAG=1` turns on a per-frame verbose line for both hops so a short
  reproduction can be timeline'd precisely.

Frontend:

- `window.__pongDiag` always collects bounded timing series: snapshot
  inter-arrival gaps, render-frame deltas, the extrapolation window left
  unfilled at draw time, the display-only corrections applied to the ball and
  each paddle, input-send cadence, WebSocket buffered amount, and transport
  choice. Call `window.__pongDiag.summary()` from a browser console, or pass
  `?diag=1` (a lobby URL `/?diag=1` also propagates it) for a `[pong-diag]`
  console summary every two seconds and on join/unload.
- A compact `[pong-diag]` row is also printed **by default** on the game page
  at join, every ~8 seconds, and on unload (and once in AI mode), so a normal
  session already leaves measurable history in the browser console without
  turning anything on. The lobby prints a one-line hint: reload as `/?diag=1`
  to upgrade every game page to a full two-second report.
- Server WebSocket state writes have a 150ms deadline. A stalled room-to-proxy
  or proxy-to-browser hop is closed instead of letting an old state frame block
  the authoritative stream for hundreds of milliseconds. The browser's
  reconnect-token path then reconnects the player. `pong_*_state_write_timeout`
  and `pong_proxy_client_write_timeout` distinguish this recovery path from
  browser rendering or simulation problems.

A healthy pipe has ~50 Hz state cadence, gap p95 near 25-30 ms, sub-millisecond
write/relay costs, tiny display corrections (< 0.02 normalized units), and an
unfilled extrapolation window close to zero. Deviation points at the exact hop:
backends ending into the room pod, the proxy, or the browser event loop.

How to read the numbers. On a throttled headless browser VM the browser's own
event loop is usually the slow limb: its render-frame deltas and input cadence
fall towards 15-20 Hz, and its slow drain inflates the *room-stream write cost*
and therefore the cadence gaps measured back to itself (TCP backpressure). The
proxy relay copy cost stays sub-millisecond even then, so it is the surest
server-side signal; the browser-side extrapolation window and corrections are
the honest end-to-end picture. A real desktop display at 60+ Hz therefore
shows far less extrapolation than the CI headless harness.

## Tests

The Go engine tests verify monotonic input handling and per-player
acknowledgements. `tests/prediction.spec.ts` uses a controlled delayed socket to
verify that the local paddle moves before another authoritative snapshot is
available and that sequence-bearing inputs are sent. The regular Playwright
room workflow continues to verify that two real clients join and exchange
playing state; the room workflow also verifies that a third client can watch a
playing room without increasing its player count. The Go proxy test verifies
that spectator disconnect does not disconnect the players or finish the room.

When changing this contract, run:

```bash
go test ./...
go test -race ./...
go vet ./...
npx playwright test --project=chromium
```
