package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/isaacphi/mcp-language-server/internal/protocol"
)

func diagnosticTestClient(t *testing.T, onBarrier ...func()) (*Client, <-chan *Message, string) {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	c := &Client{
		stdin: w, done: make(chan struct{}),
		handlers:           make(map[string]chan *Message),
		openFiles:          make(map[string]*OpenFileInfo),
		diagnostics:        make(map[protocol.DocumentUri][]protocol.Diagnostic),
		diagnosticVersions: make(map[protocol.DocumentUri]int32),
		diagnosticsChanged: make(chan struct{}),
	}
	messages := make(chan *Message, 100)
	go func() {
		reader := bufio.NewReader(r)
		for {
			msg, err := ReadMessage(reader)
			if err != nil {
				return
			}
			if msg.Method == "textDocument/hover" {
				for _, hook := range onBarrier {
					hook()
				}
				c.handlersMu.RLock()
				response := c.handlers[msg.ID.String()]
				c.handlersMu.RUnlock()
				if response != nil {
					response <- &Message{Result: json.RawMessage("null")}
				}
				continue
			}
			messages <- msg
		}
	}()
	path := filepath.Join(t.TempDir(), "sample.py")
	if err := os.WriteFile(path, []byte("value: int = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return c, messages, path
}

func TestInterimEmptyDiagnosticsBeforeBeginProgressAreNotReportedClean(t *testing.T) {
	var client *Client
	barrier := make(chan struct{}, 1)
	c, messages, path := diagnosticTestClient(t, func() {
		client.setAnalysisActive(true)
		barrier <- struct{}{}
	})
	client = c
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan []protocol.Diagnostic, 1)
	go func() {
		d, err := c.FreshDiagnostics(ctx, path)
		if err != nil {
			t.Error(err)
		}
		result <- d
	}()
	version := requestedVersion(t, messages)
	publishTestDiagnostics(c, path, version, nil)
	select {
	case <-barrier:
	case <-ctx.Done():
		t.Fatal("no ordering barrier")
	}
	select {
	case <-result:
		t.Fatal("interim empty diagnostics escaped before analysis completion")
	case <-time.After(10 * time.Millisecond):
	}
	publishTestDiagnostics(c, path, version, []protocol.Diagnostic{{Message: "real error"}})
	c.setAnalysisActive(false)
	select {
	case diagnostics := <-result:
		if len(diagnostics) != 1 || diagnostics[0].Message != "real error" {
			t.Fatalf("lost final diagnostics: %v", diagnostics)
		}
	case <-ctx.Done():
		t.Fatal("final diagnostics were not delivered")
	}
}

func publishTestDiagnostics(c *Client, path string, version int32, diagnostics []protocol.Diagnostic) {
	data, _ := json.Marshal(protocol.PublishDiagnosticsParams{
		URI: protocol.DocumentUri("file://" + path), Version: version, Diagnostics: diagnostics,
	})
	HandleDiagnostics(c, data)
}

func requestedVersion(t *testing.T, messages <-chan *Message) int32 {
	t.Helper()
	for {
		select {
		case message := <-messages:
			if message.Method == "textDocument/didClose" {
				continue
			}
			var params struct {
				TextDocument struct {
					Version int32 `json:"version"`
				} `json:"textDocument"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Fatal(err)
			}
			return params.TextDocument.Version
		case <-time.After(time.Second):
			t.Fatal("no document notification")
		}
	}
}

func TestFreshDiagnosticsRejectsStaleAndAcceptsExplicitEmpty(t *testing.T) {
	c, messages, path := diagnosticTestClient(t)
	if err := c.OpenFile(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	oldVersion := requestedVersion(t, messages)
	publishTestDiagnostics(c, path, oldVersion, []protocol.Diagnostic{{Message: "old error"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type answer struct {
		diagnostics []protocol.Diagnostic
		err         error
	}
	result := make(chan answer, 1)
	go func() { d, err := c.FreshDiagnostics(ctx, path); result <- answer{d, err} }()
	version := requestedVersion(t, messages)
	if version <= oldVersion {
		t.Fatal("refresh must advance document version")
	}
	publishTestDiagnostics(c, path, oldVersion, nil)
	select {
	case <-result:
		t.Fatal("stale/empty cached diagnostics satisfied a fresh request")
	case <-time.After(10 * time.Millisecond):
	}
	publishTestDiagnostics(c, path, version, nil)
	got := <-result
	if got.err != nil || len(got.diagnostics) != 0 {
		t.Fatalf("explicit clean result rejected: %v", got)
	}
	publishTestDiagnostics(c, path, oldVersion, []protocol.Diagnostic{{Message: "late error"}})
	if len(c.GetFileDiagnostics(protocol.DocumentUri("file://"+path))) != 0 {
		t.Fatal("late old notification overwrote newer clean diagnostics")
	}
}

func TestFreshDiagnosticsDoesNotReportCleanOnTimeout(t *testing.T) {
	c, _, path := diagnosticTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.FreshDiagnostics(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestConcurrentOpenIsIdempotentAndDiskChangesAreSynchronous(t *testing.T) {
	c, messages, path := diagnosticTestClient(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.OpenFile(context.Background(), path); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	first := requestedVersion(t, messages)
	select {
	case <-messages:
		t.Fatal("duplicate didOpen for unchanged document")
	default:
	}
	if err := os.WriteFile(path, []byte("value: int = 'bad'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.OpenFile(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if requestedVersion(t, messages) <= first {
		t.Fatal("on-disk change did not advance version")
	}
}
