package testing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/isaacphi/mcp-language-server/internal/protocol"
	"github.com/isaacphi/mcp-language-server/internal/watcher"
)

func TestSelfIndexingServerReceivesEventsWithoutAutomaticallyOpeningFiles(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing.py")
	if err := os.WriteFile(existing, []byte("a = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mock := NewMockLSPClient()
	config := watcher.DefaultWatcherConfig()
	config.SkipAutomaticOpen = true
	config.DebounceTime = 0
	w := watcher.NewWorkspaceWatcherWithConfig(mock, config)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); w.WatchWorkspace(ctx, root) }()
	defer func() { cancel(); <-done }()
	if err := w.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	w.AddRegistrations(ctx, "test", []protocol.FileSystemWatcher{{GlobPattern: protocol.GlobPattern{Value: "**/*"}}})
	created := filepath.Join(root, "artifact.json")
	if err := os.WriteFile(created, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !mock.WaitForFileEvent(ctx, "file://"+created, protocol.FileChangeType(protocol.Created)) {
		t.Fatal("file events were disabled")
	}
	if mock.IsFileOpen(existing) || mock.IsFileOpen(created) {
		t.Fatal("watched files were force-opened")
	}
}
