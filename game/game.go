// Package game implements a server-authoritative PONG game engine.
// Coordinates are normalized to [0,1] so clients can scale to any canvas size.
package game

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// Status represents the current game phase.
type Status string

const (
	StatusWaiting  Status = "waiting"
	StatusPlaying  Status = "playing"
	StatusFinished Status = "finished"
)

// WinScore is the number of points needed to win.
const WinScore = 7

// Dimensions (normalized).
const (
	PaddleWidth   = 0.02
	PaddleHeight  = 0.15
	BallSize      = 0.025
	PaddleSpeed   = 0.025 // per tick when key held
	BaseBallSpeed = 0.012
	MaxBallSpeed  = 0.025
)

// TickDuration is the authoritative simulation interval. Keep physics at 60 Hz
// so collision and input timing stay deterministic even when network delivery
// is slower or bursty.
const TickDuration = 16 * time.Millisecond // ~60 FPS

// StateBroadcastInterval is the browser snapshot cadence. It deliberately runs
// below the simulation rate: clients render between authoritative snapshots,
// while reducing JSON/proxy work leaves more room for multiple players and
// spectators without changing the authoritative game speed.
const StateBroadcastInterval = 33 * time.Millisecond // ~30 FPS

// Ball holds the ball state.
type Ball struct {
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	DX float64 `json:"dx"`
	DY float64 `json:"dy"`
}

// Paddle holds a paddle's vertical position (center).
type Paddle struct {
	Y float64 `json:"y"`
}

// State is the full game state sent to clients each tick.
type State struct {
	Ball            Ball   `json:"ball"`
	P1              Paddle `json:"p1"`
	P2              Paddle `json:"p2"`
	Score1          int    `json:"score1"`
	Score2          int    `json:"score2"`
	Status          Status `json:"status"`
	Winner          int    `json:"winner"` // 0=none, 1=p1, 2=p2
	P1Ready         bool   `json:"p1_ready"`
	P2Ready         bool   `json:"p2_ready"`
	P1InputSequence uint64 `json:"p1_input_sequence"`
	P2InputSequence uint64 `json:"p2_input_sequence"`
}

// Input represents a player's paddle movement intent. Sequence is assigned by
// the client and echoed by State once the authoritative tick has processed it.
// It lets a client reconcile its local prediction without making the client
// authoritative.
type Input struct {
	Player   int    `json:"player"` // 1 or 2
	Up       bool   `json:"up"`
	Down     bool   `json:"down"`
	Sequence uint64 `json:"sequence"`
}

// Engine runs the authoritative game loop.
type Engine struct {
	mu     sync.Mutex
	state  State
	input1 Input // latest known input for p1
	input2 Input // latest known input for p2
}

// NewEngine creates a new game engine in waiting state.
func NewEngine() *Engine {
	e := &Engine{}
	e.state = State{
		Ball:   Ball{X: 0.5, Y: 0.5, DX: 0, DY: 0},
		P1:     Paddle{Y: 0.5},
		P2:     Paddle{Y: 0.5},
		Status: StatusWaiting,
	}
	return e
}

// State returns a copy of the current game state.
func (e *Engine) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.state
	return s
}

// ApplyInput records a player's movement intent.
func (e *Engine) ApplyInput(in Input) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch in.Player {
	case 1:
		if (in.Sequence == 0 && e.input1.Sequence != 0) ||
			(in.Sequence != 0 && in.Sequence <= e.input1.Sequence) {
			return
		}
		e.input1 = in
	case 2:
		if (in.Sequence == 0 && e.input2.Sequence != 0) ||
			(in.Sequence != 0 && in.Sequence <= e.input2.Sequence) {
			return
		}
		e.input2 = in
	}
}

// PlayerReady marks a player as ready and starts the game when both are ready.
func (e *Engine) PlayerReady(player int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if player == 1 {
		e.state.P1Ready = true
	} else {
		e.state.P2Ready = true
	}
	if e.state.P1Ready && e.state.P2Ready && e.state.Status == StatusWaiting {
		e.state.Status = StatusPlaying
		e.resetBall()
	}
}

// PlayerLeft marks a player as disconnected.
func (e *Engine) PlayerLeft(player int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Status == StatusFinished {
		return
	}
	e.state.Status = StatusFinished
	if player == 1 {
		e.state.Winner = 2
	} else {
		e.state.Winner = 1
	}
}

// Tick advances the game by one frame. Returns the updated state.
func (e *Engine) Tick() State {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state.Status != StatusPlaying {
		return e.state
	}

	// Move paddles and acknowledge the exact input intent applied by this
	// authoritative tick. Clients use this acknowledgement to replay only
	// inputs the server has not processed yet.
	e.movePaddle(&e.state.P1, &e.input1)
	e.movePaddle(&e.state.P2, &e.input2)
	e.state.P1InputSequence = e.input1.Sequence
	e.state.P2InputSequence = e.input2.Sequence

	// Move ball
	b := &e.state.Ball
	b.X += b.DX
	b.Y += b.DY

	// Top/bottom wall bounce
	if b.Y-BallSize/2 <= 0 {
		b.Y = BallSize / 2
		b.DY = math.Abs(b.DY)
	}
	if b.Y+BallSize/2 >= 1 {
		b.Y = 1 - BallSize/2
		b.DY = -math.Abs(b.DY)
	}

	// Paddle 1 collision (left side)
	if b.X-BallSize/2 <= PaddleWidth && b.DX < 0 {
		paddleCenter := e.state.P1.Y
		if b.Y >= paddleCenter-PaddleHeight/2 && b.Y <= paddleCenter+PaddleHeight/2 {
			b.X = PaddleWidth + BallSize/2
			b.DX = math.Abs(b.DX)
			// Add angle based on where ball hits paddle
			offset := (b.Y - paddleCenter) / (PaddleHeight / 2)
			b.DY += offset * 0.005
			// Clamp speed
			e.clampBallSpeed()
		}
	}

	// Paddle 2 collision (right side)
	if b.X+BallSize/2 >= 1-PaddleWidth && b.DX > 0 {
		paddleCenter := e.state.P2.Y
		if b.Y >= paddleCenter-PaddleHeight/2 && b.Y <= paddleCenter+PaddleHeight/2 {
			b.X = 1 - PaddleWidth - BallSize/2
			b.DX = -math.Abs(b.DX)
			offset := (b.Y - paddleCenter) / (PaddleHeight / 2)
			b.DY += offset * 0.005
			e.clampBallSpeed()
		}
	}

	// Scoring
	if b.X < 0 {
		e.state.Score2++
		e.checkWin()
		if e.state.Status == StatusPlaying {
			e.resetBall()
		}
	}
	if b.X > 1 {
		e.state.Score1++
		e.checkWin()
		if e.state.Status == StatusPlaying {
			e.resetBall()
		}
	}

	return e.state
}

func (e *Engine) movePaddle(p *Paddle, in *Input) {
	if in.Up {
		p.Y -= PaddleSpeed
	}
	if in.Down {
		p.Y += PaddleSpeed
	}
	// Clamp
	halfH := PaddleHeight / 2
	if p.Y < halfH {
		p.Y = halfH
	}
	if p.Y > 1-halfH {
		p.Y = 1 - halfH
	}
}

func (e *Engine) clampBallSpeed() {
	speed := math.Sqrt(e.state.Ball.DX*e.state.Ball.DX + e.state.Ball.DY*e.state.Ball.DY)
	if speed > MaxBallSpeed {
		scale := MaxBallSpeed / speed
		e.state.Ball.DX *= scale
		e.state.Ball.DY *= scale
	}
	if speed < BaseBallSpeed {
		scale := BaseBallSpeed / speed
		e.state.Ball.DX *= scale
		e.state.Ball.DY *= scale
	}
}

func (e *Engine) resetBall() {
	e.state.Ball.X = 0.5
	e.state.Ball.Y = 0.5
	// Random direction, fixed speed
	angle := (rand.Float64() - 0.5) * math.Pi / 3 // ±30 degrees
	dir := 1.0
	if rand.Intn(2) == 0 {
		dir = -1.0
	}
	e.state.Ball.DX = dir * BaseBallSpeed * math.Cos(angle)
	e.state.Ball.DY = BaseBallSpeed * math.Sin(angle)
}

func (e *Engine) checkWin() {
	if e.state.Score1 >= WinScore {
		e.state.Status = StatusFinished
		e.state.Winner = 1
	} else if e.state.Score2 >= WinScore {
		e.state.Status = StatusFinished
		e.state.Winner = 2
	}
}
