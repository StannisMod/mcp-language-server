package lsp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

type yieldingWriter struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (w *yieldingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.data.Write(p)
	w.mu.Unlock()
	runtime.Gosched()
	return n, err
}

func (w *yieldingWriter) Close() error { return nil }

func TestConcurrentFramesDoNotInterleave(t *testing.T) {
	w := &yieldingWriter{}
	c := &Client{stdin: w}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Notify(context.Background(), "test", map[string]string{"value": "payload"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r := bufio.NewReader(bytes.NewReader(w.data.Bytes()))
	for i := 0; i < 100; i++ {
		msg, err := ReadMessage(r)
		if err != nil || msg.Method != "test" {
			t.Fatalf("frame %d corrupted: %v", i, err)
		}
	}
}

func TestCallHonorsCancellationAndClosedConnection(t *testing.T) {
	for _, closed := range []bool{false, true} {
		c := &Client{stdin: &yieldingWriter{}, handlers: make(map[string]chan *Message), done: make(chan struct{})}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		if closed {
			close(c.done)
		}
		err := c.Call(ctx, "never-responds", nil, nil)
		cancel()
		if err == nil || (!closed && !errors.Is(err, context.DeadlineExceeded)) {
			t.Fatalf("expected bounded failure, got %v", err)
		}
		if len(c.handlers) != 0 {
			t.Fatal("pending request was not removed")
		}
	}
}
