package audio

import (
	"sync"
	"testing"
	"time"
)

func mkFrame(i int) Frame {
	return Frame{
		PCM:       []byte{byte(i)},
		Timestamp: time.Unix(int64(i), 0),
	}
}

func TestRingBuffer_BasicPushSnapshot(t *testing.T) {
	rb := NewRingBuffer(4)
	if rb.Len() != 0 {
		t.Errorf("initial Len: got %d want 0", rb.Len())
	}
	for i := 1; i <= 3; i++ {
		rb.Push(mkFrame(i))
	}
	if rb.Len() != 3 {
		t.Errorf("Len after 3 pushes: got %d want 3", rb.Len())
	}
	got := rb.Snapshot()
	if len(got) != 3 {
		t.Fatalf("Snapshot len: got %d want 3", len(got))
	}
	for i, f := range got {
		if f.PCM[0] != byte(i+1) {
			t.Errorf("frame %d: got %d want %d", i, f.PCM[0], i+1)
		}
	}
}

func TestRingBuffer_OverflowEvicts(t *testing.T) {
	rb := NewRingBuffer(3)
	for i := 1; i <= 5; i++ {
		rb.Push(mkFrame(i))
	}
	if rb.Len() != 3 {
		t.Errorf("Len: got %d want 3", rb.Len())
	}
	got := rb.Snapshot()
	want := []byte{3, 4, 5}
	for i, f := range got {
		if f.PCM[0] != want[i] {
			t.Errorf("frame %d: got %d want %d", i, f.PCM[0], want[i])
		}
	}
}

func TestRingBuffer_Since(t *testing.T) {
	rb := NewRingBuffer(10)
	for i := 1; i <= 5; i++ {
		rb.Push(mkFrame(i))
	}
	got := rb.Since(time.Unix(3, 0))
	if len(got) != 3 {
		t.Fatalf("Since(3): len got %d want 3", len(got))
	}
	for i, f := range got {
		if f.PCM[0] != byte(i+3) {
			t.Errorf("frame %d: got %d want %d", i, f.PCM[0], i+3)
		}
	}
	// Future timestamp returns empty.
	if got := rb.Since(time.Unix(100, 0)); got != nil {
		t.Errorf("Since(future): got %v want nil", got)
	}
}

func TestRingBuffer_Reset(t *testing.T) {
	rb := NewRingBuffer(4)
	for i := 1; i <= 3; i++ {
		rb.Push(mkFrame(i))
	}
	rb.Reset()
	if rb.Len() != 0 {
		t.Errorf("Len after Reset: got %d want 0", rb.Len())
	}
	if got := rb.Snapshot(); got != nil {
		t.Errorf("Snapshot after Reset: got %v want nil", got)
	}
	rb.Push(mkFrame(99))
	if rb.Len() != 1 {
		t.Errorf("Push after Reset: Len got %d want 1", rb.Len())
	}
}

func TestRingBuffer_ConcurrentPushSnapshot(t *testing.T) {
	rb := NewRingBuffer(64)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 1000 {
			rb.Push(mkFrame(i))
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			_ = rb.Snapshot()
		}
	}()
	wg.Wait()
	if rb.Len() == 0 || rb.Len() > rb.Capacity() {
		t.Errorf("Len out of range: %d", rb.Len())
	}
}

func TestCapacityForSeconds(t *testing.T) {
	if got := CapacityForSeconds(30); got != 375 {
		t.Errorf("30s: got %d want 375", got)
	}
	if got := CapacityForSeconds(0); got != 1 {
		t.Errorf("0s: got %d want 1 (degraded floor)", got)
	}
	if got := CapacityForSeconds(1); got != 12 {
		t.Errorf("1s: got %d want 12", got)
	}
}
