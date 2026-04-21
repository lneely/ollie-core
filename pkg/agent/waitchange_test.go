package agent

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	olog "ollie/pkg/log"
)

// newTestCore returns a minimal agentCore with changeCond wired up.
func newTestCore(initialState string) *agentCore {
	a := &agentCore{
		state: initialState,
		log:   olog.NewWriter("test", olog.LevelError+1, io.Discard, io.Discard),
	}
	a.changeCond = sync.NewCond(&a.changeMu)
	return a
}

// TestWaitChange_ReturnOnChange verifies that WaitChange unblocks when
// setState is called with a different value.
func TestWaitChange_ReturnOnChange(t *testing.T) {
	a := newTestCore("idle")

	result := make(chan string, 1)
	go func() {
		v, ok := a.WaitChange(context.Background(), WatchState, "idle")
		if !ok {
			result <- "!ok"
			return
		}
		result <- v
	}()

	time.Sleep(10 * time.Millisecond) // let goroutine reach cond.Wait
	a.setState("thinking")

	select {
	case got := <-result:
		if got != "thinking" {
			t.Errorf("WaitChange returned %q; want %q", got, "thinking")
		}
	case <-time.After(time.Second):
		t.Fatal("WaitChange did not unblock after setState")
	}
}

// TestWaitChange_AlreadyChanged verifies that if the value has already
// changed before WaitChange is called, it returns immediately.
func TestWaitChange_AlreadyChanged(t *testing.T) {
	a := newTestCore("thinking")

	done := make(chan struct{})
	go func() {
		v, ok := a.WaitChange(context.Background(), WatchState, "idle")
		if !ok || v != "thinking" {
			t.Errorf("WaitChange returned (%q, %v); want (\"thinking\", true)", v, ok)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WaitChange blocked when value already changed")
	}
}

// TestWaitChange_ContextCancel verifies that WaitChange returns ("", false)
// when the context is cancelled.
func TestWaitChange_ContextCancel(t *testing.T) {
	a := newTestCore("idle")

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	go func() {
		_, ok := a.WaitChange(ctx, WatchState, "idle")
		result <- ok
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case ok := <-result:
		if ok {
			t.Error("WaitChange returned ok=true after context cancel; want false")
		}
	case <-time.After(time.Second):
		t.Fatal("WaitChange did not unblock after context cancel")
	}
}

// TestWaitChange_FullCycle simulates idle→thinking→idle and checks each
// transition is observed in order.
func TestWaitChange_FullCycle(t *testing.T) {
	a := newTestCore("idle")

	// Step 1: wait for idle→thinking
	thinking := make(chan string, 1)
	go func() {
		v, _ := a.WaitChange(context.Background(), WatchState, "idle")
		thinking <- v
	}()

	time.Sleep(10 * time.Millisecond)
	a.setState("thinking")

	var got string
	select {
	case got = <-thinking:
	case <-time.After(time.Second):
		t.Fatal("did not observe idle→thinking")
	}
	if got != "thinking" {
		t.Errorf("step1: got %q; want \"thinking\"", got)
	}

	// Step 2: wait for thinking→idle
	idle := make(chan string, 1)
	go func() {
		v, _ := a.WaitChange(context.Background(), WatchState, "thinking")
		idle <- v
	}()

	time.Sleep(10 * time.Millisecond)
	a.setState("idle")

	select {
	case got = <-idle:
	case <-time.After(time.Second):
		t.Fatal("did not observe thinking→idle")
	}
	if got != "idle" {
		t.Errorf("step2: got %q; want \"idle\"", got)
	}
}

// TestWaitChange_NoMissedWakeup fires setState concurrently with WaitChange
// to stress the missed-wakeup scenario.
func TestWaitChange_NoMissedWakeup(t *testing.T) {
	const rounds = 500
	for i := 0; i < rounds; i++ {
		a := newTestCore("idle")
		done := make(chan struct{})
		go func() {
			a.WaitChange(context.Background(), WatchState, "idle") //nolint:errcheck
			close(done)
		}()
		// setState races with WaitChange entering the wait loop.
		a.setState("thinking")
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("round %d: WaitChange missed wakeup", i)
		}
	}
}
