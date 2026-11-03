package lsp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/isaacphi/mcp-language-server/internal/protocol"
)

// FileDiagnostics keeps the upstream behavior for other language servers. The
// versioned fast path is enabled for the Pyright family. Other language servers
// retain their previous diagnostics behavior rather than assuming this protocol.
func (c *Client) FileDiagnostics(ctx context.Context, path string) ([]protocol.Diagnostic, error) {
	if strings.Contains(strings.ToLower(c.Cmd.Path), "pyright") {
		return c.FreshDiagnostics(ctx, path)
	}
	if err := c.OpenFile(ctx, path); err != nil {
		return nil, err
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("LSP connection closed while waiting for diagnostics")
	}
	uri := protocol.DocumentUri("file://" + path)
	_, err := c.Diagnostic(ctx, protocol.DocumentDiagnosticParams{TextDocument: protocol.TextDocumentIdentifier{URI: uri}})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		lspLogger.Debug("Using published diagnostics: %v", err)
	}
	return c.GetFileDiagnostics(uri), nil
}

// syncFile orders document versions and writes together, including watcher edits.
// Refresh requests advance the version even for unchanged text. Callers must also
// wait for analysis completion: a versioned result can still be an interim result.
func (c *Client) syncFile(ctx context.Context, path string, refresh bool) (int32, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	uri := protocol.DocumentUri("file://" + abs)
	c.openFilesMu.Lock()
	defer c.openFilesMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return 0, fmt.Errorf("error reading file: %w", err)
	}
	hash := sha256.Sum256(content)
	previous := c.openFiles[string(uri)]
	if previous != nil && previous.ContentHash == hash && !refresh {
		return previous.Version, nil
	}
	version := c.documentVersion.Add(1)
	openParams := protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri, LanguageID: DetectLanguageID(string(uri)), Version: version, Text: string(content)},
	}
	if previous == nil {
		err = c.Notify(ctx, "textDocument/didOpen", openParams)
	} else if refresh && previous.ContentHash == hash {
		// A no-op didChange forces Pyright to discard its type evaluator and
		// recheck importers. Close/reopen republishes the current diagnostics at
		// a new version while retaining analysis for unchanged on-disk content.
		closed, closeErr := NewNotification("textDocument/didClose", protocol.DidCloseTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: uri}})
		if closeErr != nil {
			return 0, closeErr
		}
		opened, openErr := NewNotification("textDocument/didOpen", openParams)
		if openErr != nil {
			return 0, openErr
		}
		err = c.writeMessages(closed, opened)
	} else {
		err = c.Notify(ctx, "textDocument/didChange", protocol.DidChangeTextDocumentParams{
			TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri}, Version: version},
			ContentChanges: []protocol.TextDocumentContentChangeEvent{{Value: protocol.TextDocumentContentChangeWholeDocument{Text: string(content)}}},
		})
	}
	if err != nil {
		return 0, err
	}
	c.openFiles[string(uri)] = &OpenFileInfo{Version: version, URI: uri, ContentHash: hash}
	return version, nil
}

// FreshDiagnostics waits for a versioned push result instead of sleeping or
// sending textDocument/diagnostic, which Pyright 1.1.390 does not support.
func (c *Client) FreshDiagnostics(ctx context.Context, path string) ([]protocol.Diagnostic, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := protocol.DocumentUri("file://" + abs)
	// Imported documents already open in the server are editor-owned buffers.
	// Synchronize them too, without waiting for the filesystem watcher's debounce.
	c.openFilesMu.RLock()
	paths := make([]string, 0, len(c.openFiles))
	for _, info := range c.openFiles {
		if info.URI != uri {
			paths = append(paths, info.URI.Path())
		}
	}
	c.openFilesMu.RUnlock()
	for _, openPath := range paths {
		if _, err := c.syncFile(ctx, openPath, false); err != nil {
			if os.IsNotExist(err) {
				if err := c.CloseFile(ctx, openPath); err != nil {
					return nil, err
				}
			} else {
				return nil, err
			}
		}
	}
	version, err := c.syncFile(ctx, abs, true)
	if err != nil {
		return nil, err
	}
	var barrierVersion int32
	for {
		c.diagnosticsMu.RLock()
		reported, exists := c.diagnosticVersions[uri]
		if exists && reported >= version && !c.analysisActive {
			if barrierVersion < reported {
				c.diagnosticsMu.RUnlock()
				// Pyright may publish interim empty diagnostics immediately BEFORE
				// beginProgress in the same callback. A standard request/response
				// barrier drains those ordered notifications before trusting idle.
				params := protocol.HoverParams{}
				params.TextDocument = protocol.TextDocumentIdentifier{URI: uri}
				if err := c.Call(ctx, "textDocument/hover", params, nil); err != nil {
					return nil, err
				}
				barrierVersion = reported
				continue
			}
			result := append([]protocol.Diagnostic(nil), c.diagnostics[uri]...)
			c.diagnosticsMu.RUnlock()
			c.openFilesMu.RLock()
			info := c.openFiles[string(uri)]
			if info == nil {
				c.openFilesMu.RUnlock()
				return nil, fmt.Errorf("document closed during diagnostics")
			}
			hash, currentVersion := info.ContentHash, info.Version
			c.openFilesMu.RUnlock()
			content, err := os.ReadFile(abs)
			if err != nil {
				return nil, err
			}
			if sha256.Sum256(content) != hash {
				version, err = c.syncFile(ctx, abs, true)
				if err != nil {
					return nil, err
				}
				continue
			}
			if currentVersion > reported {
				version = currentVersion
				continue
			}
			return result, nil
		}
		changed := c.diagnosticsChanged
		c.diagnosticsMu.RUnlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for diagnostics version %d: %w", version, ctx.Err())
		case <-c.done:
			return nil, fmt.Errorf("LSP connection closed while waiting for diagnostics")
		}
	}
}

func (c *Client) setAnalysisActive(active bool) {
	c.diagnosticsMu.Lock()
	defer c.diagnosticsMu.Unlock()
	c.analysisActive = active
	close(c.diagnosticsChanged)
	c.diagnosticsChanged = make(chan struct{})
}
