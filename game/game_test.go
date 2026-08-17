package game

import (
	"testing"
	"time"
)

func TestStateBroadcastIsSlowerThanAuthoritativeSimulation(t *testing.T) {
	if StateBroadcastInterval <= TickDuration {
		t.Fatalf("state broadcast interval = %s, want slower than simulation tick %s", StateBroadcastInterval, TickDuration)
	}
	if StateBroadcastInterval > 50*time.Millisecond {
		t.Fatalf("state broadcast interval = %s, want no more than 20 FPS latency", StateBroadcastInterval)
	}
}

func TestApplyInputRejectsOutOfOrderSequences(t *testing.T) {
	engine := NewEngine()
	engine.PlayerReady(1)
	engine.PlayerReady(2)

	engine.ApplyInput(Input{Player: 1, Down: true, Sequence: 2})
	engine.ApplyInput(Input{Player: 1, Up: true, Sequence: 1})
	state := engine.Tick()

	if state.P1.Y <= 0.5 {
		t.Fatalf("p1 y = %v, want a downward move from the newest input", state.P1.Y)
	}
	if state.P1InputSequence != 2 {
		t.Fatalf("p1 input sequence = %d, want 2", state.P1InputSequence)
	}
}

func TestLegacyInputCannotOverwriteSequencedIntent(t *testing.T) {
	engine := NewEngine()
	engine.PlayerReady(1)
	engine.PlayerReady(2)

	engine.ApplyInput(Input{Player: 1, Down: true, Sequence: 4})
	engine.ApplyInput(Input{Player: 1, Up: true})
	state := engine.Tick()

	if state.P1.Y <= 0.5 || state.P1InputSequence != 4 {
		t.Fatalf("legacy input changed sequenced intent: y=%v sequence=%d", state.P1.Y, state.P1InputSequence)
	}
}

func TestTickAcknowledgesLatestInputForEachPlayer(t *testing.T) {
	engine := NewEngine()
	engine.PlayerReady(1)
	engine.PlayerReady(2)

	withoutInput := engine.Tick()
	if withoutInput.P1InputSequence != 0 || withoutInput.P2InputSequence != 0 {
		t.Fatalf("initial acknowledgements = %d/%d, want 0/0", withoutInput.P1InputSequence, withoutInput.P2InputSequence)
	}

	engine.ApplyInput(Input{Player: 1, Up: true, Sequence: 7})
	engine.ApplyInput(Input{Player: 2, Down: true, Sequence: 11})
	withInput := engine.Tick()
	if withInput.P1InputSequence != 7 || withInput.P2InputSequence != 11 {
		t.Fatalf("acknowledgements = %d/%d, want 7/11", withInput.P1InputSequence, withInput.P2InputSequence)
	}

	withoutNewInput := engine.Tick()
	if withoutNewInput.P1InputSequence != 7 || withoutNewInput.P2InputSequence != 11 {
		t.Fatalf("retained acknowledgements = %d/%d, want 7/11", withoutNewInput.P1InputSequence, withoutNewInput.P2InputSequence)
	}
}
