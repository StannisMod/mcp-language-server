"""Exercise real Pyright, including immediate edits and dependency freshness.

Usage: python3 scripts/python_smoke.py --server ./mcp-language-server --lsp /path/to/pyright-langserver
All writes are confined to a new temporary workspace.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import tempfile

from mcp_client import MCPClient


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", required=True)
    parser.add_argument("--lsp", required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="mcp-python-smoke-") as temporary:
        root = Path(temporary)
        (root / "pyrightconfig.json").write_text('{"typeCheckingMode":"basic"}\n')
        sample = root / "sample.py"
        original = 'def greet(name: str) -> str:\n    return "Hello " + name\n\nmessage = greet("world")\nbroken: int = "wrong"\n'
        sample.write_text(original)
        large = root / "large.py"
        large.write_text("".join(f"value_{i}: int = {i}\n" for i in range(3000)) + "broken: int = 'bad'\n")
        (root / "other.py").write_text("def greet(name: str) -> str:\n    return 'other module'\n")
        (root / "src").mkdir()
        (root / "src/alternate.py").write_text("class Example:\n    pass\n")
        for suffix in ("open", "closed"):
            (root / f"dep_{suffix}.py").write_text("value: int = 1\n")
            (root / f"use_{suffix}.py").write_text(f"from dep_{suffix} import value\nanswer: int = value\n")
        with MCPClient(args.server, root, args.lsp) as client:
            def diagnostics(file):
                return client.call("diagnostics", {"filePath": str(file)})
            def is_clean(text):
                return text.startswith("No diagnostics found")

            assert "Diagnostics in File: 1" in diagnostics(large), "interim empty diagnostics escaped before full-file analysis"
            assert "greet" in client.call("hover", {"filePath": str(sample), "line": 1, "column": 6})
            definition = client.call("definition", {"symbolName": "sample.greet"})
            assert "def greet" in definition and "other.py" not in definition
            assert "class Example" in client.call("definition", {"symbolName": "alternate.Example"}), "nonstandard source-layout fallback failed"
            assert "sample.py" in client.call("references", {"symbolName": "greet"})
            assert not is_clean(diagnostics(sample)), "cold request missed a real error"
            for _ in range(4):
                sample.write_text(original.replace('broken: int = "wrong"', "broken: int = 1"))
                assert is_clean(diagnostics(sample)), "immediate fix returned stale errors"
                sample.write_text(original)
                assert not is_clean(diagnostics(sample)), "immediate edit returned stale clean diagnostics"
            for suffix in ("open", "closed"):
                dependency = root / f"dep_{suffix}.py"
                consumer = root / f"use_{suffix}.py"
                if suffix == "open":
                    client.call("hover", {"filePath": str(dependency), "line": 1, "column": 1})
                assert is_clean(diagnostics(consumer))
                dependency.write_text("value: str = 'changed'\n")
                assert not is_clean(diagnostics(consumer)), f"stale {suffix} dependency"
                dependency.write_text("value: int = 1\n")
                assert is_clean(diagnostics(consumer)), f"stale fixed {suffix} dependency"
            with ThreadPoolExecutor(max_workers=16) as pool:
                results = list(pool.map(lambda _: diagnostics(sample), range(16)))
            assert all(not is_clean(text) for text in results), "concurrent checks lost diagnostics"
            client.call("edit_file", {"filePath": str(sample), "edits": [{"startLine": 5, "endLine": 5, "newText": "broken: int = 1"}]})
            assert is_clean(diagnostics(sample)), "MCP edit was not synchronized"
            client.call("rename_symbol", {"filePath": str(sample), "line": 1, "column": 6, "newName": "welcome"})
            assert "def welcome" in sample.read_text() and 'welcome("world")' in sample.read_text()
            assert "def welcome" in client.call("definition", {"symbolName": "sample.welcome"}), "definition reused pre-rename symbols"
            assert is_clean(diagnostics(sample))
            try:
                diagnostics(root / "does-not-exist.py")
            except RuntimeError:
                pass
            else:
                raise AssertionError("missing file must fail, not report clean diagnostics")
        assert not client.forced_shutdown and client.process.returncode == 0, "stdio EOF did not shut down cleanly"
        print("PASS: navigation, fresh diagnostics, immediate edits, open/closed dependencies, 16 concurrent checks, MCP edit/rename, missing-file failure")


if __name__ == "__main__":
    main()
