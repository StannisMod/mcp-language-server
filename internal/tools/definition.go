package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/isaacphi/mcp-language-server/internal/lsp"
	"github.com/isaacphi/mcp-language-server/internal/protocol"
)

func ReadDefinition(ctx context.Context, client *lsp.Client, symbolName string) (string, error) {
	query, module := definitionQuery(client.Cmd.Path, symbolName)
	results, knownSymbols := qualifiedPythonSymbols(ctx, client, query, module)
	if len(results) == 0 {
		symbolResult, err := client.Symbol(ctx, protocol.WorkspaceSymbolParams{Query: query})
		if err != nil {
			return "", fmt.Errorf("failed to fetch symbol: %v", err)
		}
		results, err = symbolResult.Results()
		if err != nil {
			return "", fmt.Errorf("failed to parse results: %v", err)
		}
	}

	var definitions []string
	for _, symbol := range results {
		if module != "" && !matchesPythonModule(symbol.GetLocation().URI.Path(), module) {
			continue
		}
		kind := ""
		container := ""

		// Skip symbols that we are not looking for. workspace/symbol may return
		// a large number of fuzzy matches.
		switch v := symbol.(type) {
		case *protocol.SymbolInformation:
			// SymbolInformation results have richer data.
			kind = fmt.Sprintf("Kind: %s\n", protocol.TableKindMap[v.Kind])
			if v.ContainerName != "" {
				container = fmt.Sprintf("Container Name: %s\n", v.ContainerName)
			}

			// Handle different matching strategies based on the search term
			if strings.Contains(query, ".") {
				// For qualified names like "Type.Method", require exact match
				if symbol.GetName() != query {
					continue
				}
			} else {
				// For unqualified names like "Method"
				if v.Kind == protocol.Method {
					// For methods, only match if the method name matches exactly Type.symbolName or Type::symbolName or symbolName
					if !strings.HasSuffix(symbol.GetName(), "::"+query) && !strings.HasSuffix(symbol.GetName(), "."+query) && symbol.GetName() != query {
						continue
					}
				} else if symbol.GetName() != query {
					// For non-methods, exact match only
					continue
				}
			}
		default:
			if symbol.GetName() != query {
				continue
			}
		}

		toolsLogger.Debug("Found symbol: %s", symbol.GetName())
		loc := symbol.GetLocation()

		err := client.OpenFile(ctx, loc.URI.Path())
		if err != nil {
			toolsLogger.Error("Error opening file: %v", err)
			continue
		}

		banner := "---\n\n"
		var definition string
		if snapshot, exists := knownSymbols[loc.URI]; exists {
			if snapshot.hash != client.OpenDocumentHash(loc.URI) {
				return "", fmt.Errorf("document changed during definition lookup; retry the request")
			}
			definition, loc, err = fullDefinitionFromSymbols(loc, snapshot.symbols)
		} else {
			definition, loc, err = GetFullDefinition(ctx, client, loc)
		}
		locationInfo := fmt.Sprintf(
			"Symbol: %s\n"+
				"File: %s\n"+
				kind+
				container+
				"Range: L%d:C%d - L%d:C%d\n\n",
			symbol.GetName(),
			strings.TrimPrefix(string(loc.URI), "file://"),
			loc.Range.Start.Line+1,
			loc.Range.Start.Character+1,
			loc.Range.End.Line+1,
			loc.Range.End.Character+1,
		)

		if err != nil {
			toolsLogger.Error("Error getting definition: %v", err)
			continue
		}

		definition = addLineNumbers(definition, int(loc.Range.Start.Line)+1)

		definitions = append(definitions, banner+locationInfo+definition+"\n")
	}

	if len(definitions) == 0 {
		return fmt.Sprintf("%s not found", symbolName), nil
	}

	return strings.Join(definitions, ""), nil
}

// Pyright workspace/symbol searches short names, not Python import paths.
// Restrict the results to the requested module instead of returning namesakes.
func definitionQuery(lspCommand, symbolName string) (string, string) {
	if strings.Contains(strings.ToLower(lspCommand), "pyright") {
		if index := strings.LastIndex(symbolName, "."); index > 0 {
			return symbolName[index+1:], strings.ReplaceAll(symbolName[:index], ".", "/")
		}
	}
	return symbolName, ""
}

func matchesPythonModule(path, module string) bool {
	return strings.HasSuffix(path, "/"+module+".py") ||
		strings.HasSuffix(path, "/"+module+".pyi") ||
		strings.HasSuffix(path, "/"+module+"/__init__.py") ||
		strings.HasSuffix(path, "/"+module+"/__init__.pyi")
}

// A fully qualified Python name already identifies its module. Query that
// document directly instead of scanning the whole workspace, without keeping a
// stale symbol/result cache. Nonstandard source layouts retain workspace search.
type documentSymbolSnapshot struct {
	hash    [32]byte
	symbols []protocol.DocumentSymbolResult
}

func qualifiedPythonSymbols(ctx context.Context, client *lsp.Client, query, module string) ([]protocol.WorkspaceSymbolResult, map[protocol.DocumentUri]documentSymbolSnapshot) {
	known := make(map[protocol.DocumentUri]documentSymbolSnapshot)
	var result []protocol.WorkspaceSymbolResult
	if module == "" || client.WorkspaceDir == "" {
		return result, known
	}
	for _, suffix := range []string{".py", ".pyi", "/__init__.py", "/__init__.pyi"} {
		path := filepath.Join(client.WorkspaceDir, filepath.FromSlash(module+suffix))
		rel, err := filepath.Rel(client.WorkspaceDir, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := client.OpenFile(ctx, path); err != nil {
			continue
		}
		uri := protocol.DocumentUri("file://" + path)
		hash := client.OpenDocumentHash(uri)
		symbolsResult, err := client.DocumentSymbol(ctx, protocol.DocumentSymbolParams{TextDocument: protocol.TextDocumentIdentifier{URI: uri}})
		if err != nil {
			continue
		}
		symbols, err := symbolsResult.Results()
		if err != nil {
			continue
		}
		known[uri] = documentSymbolSnapshot{hash: hash, symbols: symbols}
		for _, symbol := range symbols {
			if symbol.GetName() != query {
				continue
			}
			switch symbol := symbol.(type) {
			case *protocol.SymbolInformation:
				result = append(result, symbol)
			case *protocol.DocumentSymbol:
				result = append(result, &protocol.SymbolInformation{Name: symbol.Name, Kind: symbol.Kind, Location: protocol.Location{URI: uri, Range: symbol.Range}})
			}
		}
	}
	return result, known
}
