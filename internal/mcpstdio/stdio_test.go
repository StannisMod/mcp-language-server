package mcpstdio

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestParallelDispatchCancellationAndCompleteFrames(t *testing.T) {
	srv := server.NewMCPServer("test", "1")
	entered := make(chan string, 10)
	release := make(chan struct{})
	srv.AddTool(mcp.NewTool("test"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		key, _ := req.Params.Arguments["key"].(string)
		entered <- key
		select {
		case <-release:
			return mcp.NewToolResultText(key), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	input, clientWriter := io.Pipe()
	clientReader, output := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = input.Close(); _ = clientWriter.Close(); _ = clientReader.Close(); _ = output.Close() }()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, input, output, 2) }()
	responses := make(chan map[string]json.RawMessage, 20)
	go func() {
		scanner := bufio.NewScanner(clientReader)
		for scanner.Scan() {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
				t.Error(err)
				return
			}
			responses <- m
		}
	}()
	var writeMu sync.Mutex
	send := func(m map[string]any) {
		m["jsonrpc"] = "2.0"
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := json.NewEncoder(clientWriter).Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	waitResponse := func() map[string]json.RawMessage {
		select {
		case m := <-responses:
			return m
		case <-time.After(time.Second):
			t.Fatal("response timeout")
		}
		return nil
	}
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}}})
	if got := waitResponse(); string(got["id"]) != "1" {
		t.Fatalf("initialize failed: %s", got)
	}
	send(map[string]any{"method": "notifications/initialized"})
	for _, key := range []string{"first", "second", "queued"} {
		send(map[string]any{"id": key, "method": "tools/call", "params": map[string]any{"name": "test", "arguments": map[string]string{"key": key}}})
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("tools were serialized")
		}
	}
	// A slow tool must not prevent control traffic or cancellation.
	send(map[string]any{"id": 2, "method": "ping"})
	if got := waitResponse(); string(got["id"]) != "2" {
		t.Fatalf("ping blocked: %s", got)
	}
	send(map[string]any{"method": "notifications/cancelled", "params": map[string]any{"requestId": "first"}})
	got := waitResponse()
	if string(got["id"]) != `"first"` {
		t.Fatalf("cancellation failed: %s", got)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cancelled worker was not released")
	}
	close(release)
	seen := make(map[string]bool)
	for i := 0; i < 2; i++ {
		got := waitResponse()
		seen[string(got["id"])] = true
	}
	if !seen[`"second"`] || !seen[`"queued"`] {
		t.Fatalf("corrupted/lost concurrent responses: %v", seen)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("transport did not stop")
	}
}
