package agent

import (
	"sync"
	"testing"
)

func TestPromptFIFO_BasicOrder(t *testing.T) {
	var f PromptFIFO
	f.Push("a")
	f.Push("b")
	f.Push("c")

	for _, want := range []string{"a", "b", "c"} {
		got, ok := f.Pop()
		if !ok {
			t.Fatalf("Pop() = _, false; want %q", want)
		}
		if got != want {
			t.Errorf("Pop() = %q; want %q", got, want)
		}
	}

	if got, ok := f.Pop(); ok {
		t.Errorf("Pop() on empty = %q, true; want \"\", false", got)
	}
}

func TestPromptFIFO_EmptyPop(t *testing.T) {
	var f PromptFIFO
	if _, ok := f.Pop(); ok {
		t.Error("Pop() on zero-value FIFO returned ok=true")
	}
}

func TestPromptFIFO_ConcurrentPushPop(t *testing.T) {
	var f PromptFIFO
	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Push("x")
		}()
	}
	wg.Wait()

	count := 0
	for {
		_, ok := f.Pop()
		if !ok {
			break
		}
		count++
	}
	if count != n {
		t.Errorf("after %d concurrent pushes, popped %d items; want %d", n, count, n)
	}
}
