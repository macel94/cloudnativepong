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

The engine advances on its fixed `game.TickDuration` (currently 16 ms). It
accepts paddle **intent** (`up`/`down`), never a client-supplied position. A
client cannot move a paddle, ball, or score by sending a fabricated coordinate.

The browser-facing room connection sends authoritative snapshots and accepts
input over both WebSocket and the optional WebTransport stream. Both
transports use the same JSON game protocol.

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

The browser does not render directly from the arrival of a server tick.
`requestAnimationFrame` owns drawing and continues at the display's cadence.

- **Local paddle:** client-side predicted. The current held input is applied
  immediately on animation frames, independently of state delivery. When a
  snapshot acknowledges input, the browser uses the authoritative paddle as a
  reconciliation baseline, forecasts the short presentation delay, and eases
  any correction over a bounded window. The server remains authoritative; this
  is only a visual prediction.
- **Other paddle:** snapshot-interpolated. The browser keeps a short buffer and
  renders the opponent between two received authoritative snapshots. This
  intentionally shows the opponent slightly in the past so packet timing does
  not produce visible jumps.
- **Ball:** snapshot-interpolated from authoritative positions. It is not
  locally simulated for online play, because collisions and scoring must remain
  exactly server-controlled. A packet burst can therefore make the ball's
  presentation marginally behind the server, but it remains smooth and does
  not block the render loop.
- **Score and game status:** updated from the newest authoritative snapshot,
  not from predicted presentation state.

The result is that a player sees their own paddle in the present, while the
opponent and ball are rendered from a smooth, very short historical window.
The small visual difference is preferable to input lag or tick-driven
stuttering and has no effect on the authoritative outcome.

## Reconciliation and boundaries

Prediction is intentionally conservative:

- paddle positions are clamped to the same normalized bounds as the server;
- pending input acknowledgements are bounded in memory;
- correction is eased rather than allowed to accumulate indefinitely;
- the latest server snapshot can always correct a client prediction; and
- prediction stops affecting authoritative state when the connection closes or
  the server reports a finished game.

This is not a rollback or client-authoritative physics system. The ball is not
predicted online, and clients never decide whether a paddle collision or point
happened.

## Tests

The Go engine tests verify monotonic input handling and per-player
acknowledgements. `tests/prediction.spec.ts` uses a controlled delayed socket to
verify that the local paddle moves before another authoritative snapshot is
available and that sequence-bearing inputs are sent. The regular Playwright
room workflow continues to verify that two real clients join and exchange
playing state.

When changing this contract, run:

```bash
go test ./...
go test -race ./...
go vet ./...
npx playwright test --project=chromium
```
