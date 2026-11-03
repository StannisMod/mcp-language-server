package tools

import "testing"

func TestPythonQualifiedDefinitionQuery(t *testing.T) {
	query, module := definitionQuery("/project/.venv/bin/pyright-langserver", "acme.services.writer.SourceReading")
	if query != "SourceReading" || module != "acme/services/writer" {
		t.Fatalf("unexpected query: %q, module: %q", query, module)
	}
	if !matchesPythonModule("/project/"+module+".py", module) {
		t.Fatal("definition in requested module must match")
	}
	if matchesPythonModule("/project/other/writer.py", module) {
		t.Fatal("a namesake in another module must not match")
	}
	query, module = definitionQuery("/bin/gopls", "pkg.Method")
	if query != "pkg.Method" || module != "" {
		t.Fatal("non-Python queries must retain their behavior")
	}
}
