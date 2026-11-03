// Package mcpstdio adds bounded concurrent tool dispatch to mcp-go's stdio
// transport. Protocol handling remains in MCPServer; complete output messages
// are serialized, and initialization/control messages are handled in order.
package mcpstdio

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type session struct {
	initialized   atomic.Bool
	notifications chan mcp.JSONRPCNotification
}

func (s *session) SessionID() string                                   { return "stdio" }
func (s *session) Initialize()                                         { s.initialized.Store(true) }
func (s *session) Initialized() bool                                   { return s.initialized.Load() }
func (s *session) NotificationChannel() chan<- mcp.JSONRPCNotification { return s.notifications }

type job struct {
	id      string
	message json.RawMessage
	ctx     context.Context
	cancel  context.CancelFunc
}

// Serve runs at most workers tool calls concurrently. Callers can independently
// cancel queued/running requests with the standard notifications/cancelled.
func Serve(ctx context.Context, srv *server.MCPServer, input io.Reader, output io.Writer, workers int) error {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	session := &session{notifications: make(chan mcp.JSONRPCNotification, 100)}
	if err := srv.RegisterSession(ctx, session); err != nil {
		return err
	}
	defer srv.UnregisterSession(ctx, session.SessionID())
	ctx = srv.WithContext(ctx, session)
	var outputMu sync.Mutex
	var outputErr error
	encode := json.NewEncoder(output)
	write := func(response any) {
		if response == nil {
			return
		}
		outputMu.Lock()
		defer outputMu.Unlock()
		if outputErr == nil {
			outputErr = encode.Encode(response)
			if outputErr != nil {
				cancel()
			}
		}
	}
	var pendingMu sync.Mutex
	pending := make(map[string]context.CancelFunc)
	jobs := make(chan job, 64)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if j.ctx.Err() == nil {
					write(srv.HandleMessage(j.ctx, j.message))
				} else {
					write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(j.id), "error": map[string]any{"code": -32800, "message": "Request cancelled"}})
				}
				pendingMu.Lock()
				delete(pending, j.id)
				pendingMu.Unlock()
				j.cancel()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case n := <-session.notifications:
				write(n)
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		cancel()
		close(jobs)
		wg.Wait()
	}()
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		reader := bufio.NewReader(input)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			outputMu.Lock()
			err := outputErr
			outputMu.Unlock()
			if err != nil {
				return err
			}
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				err := <-readErr
				if err == io.EOF {
					return nil
				}
				return err
			}
			var header struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					RequestID json.RawMessage `json:"requestId"`
				} `json:"params"`
			}
			if json.Unmarshal(line, &header) != nil {
				write(srv.HandleMessage(ctx, line))
				continue
			}
			if header.Method == "notifications/cancelled" {
				pendingMu.Lock()
				if stop := pending[string(header.Params.RequestID)]; stop != nil {
					stop()
				}
				pendingMu.Unlock()
				continue
			}
			if header.Method != "tools/call" || len(header.ID) == 0 {
				write(srv.HandleMessage(ctx, line))
				continue
			}
			id := string(header.ID)
			callCtx, stop := context.WithTimeout(ctx, 60*time.Second)
			pendingMu.Lock()
			_, duplicate := pending[id]
			if !duplicate {
				pending[id] = stop
			}
			pendingMu.Unlock()
			if duplicate {
				stop()
				write(map[string]any{"jsonrpc": "2.0", "id": header.ID, "error": map[string]any{"code": -32600, "message": "Duplicate request ID"}})
				continue
			}
			select {
			case jobs <- job{id: id, message: line, ctx: callCtx, cancel: stop}:
			default:
				pendingMu.Lock()
				delete(pending, id)
				pendingMu.Unlock()
				stop()
				write(map[string]any{"jsonrpc": "2.0", "id": header.ID, "error": map[string]any{"code": -32000, "message": "Tool queue is full; retry after pending calls complete"}})
			}
		}
	}
}
