"""Read-only cold/warm MCP benchmark; prints JSON and verifies input hashes.

Example: python3 scripts/benchmark.py --server ./mcp-language-server --lsp /path/to/pyright-langserver --workspace /project --file package/a.py --file package/b.py --symbol package.a.Example
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import statistics
import re
import time

from mcp_client import MCPClient


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", required=True)
    parser.add_argument("--lsp", required=True)
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--file", action="append", required=True)
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--rounds", type=int, default=5)
    parser.add_argument("--expect-diagnostics", nargs="+", type=int,
                        help="Expected counts in --file order; checked on every cold/warm round")
    args = parser.parse_args()
    root = Path(args.workspace).resolve()
    files = [(root / name).resolve() for name in args.file]
    if args.expect_diagnostics is not None and len(args.expect_diagnostics) != len(files):
        parser.error("--expect-diagnostics must give one count per --file")
    watched = files + [root / name for name in ("pyproject.toml", "pyrightconfig.json") if (root / name).is_file()]
    module = root / args.symbol.rsplit(".",1)[0].replace(".","/")
    for candidate in (module.with_suffix(".py"), module / "__init__.py"):
        if candidate.is_file() and candidate not in watched:
            watched.append(candidate)
    def hashes():
        return {str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in watched}
    before = hashes()
    requests = [("diagnostics", {"filePath": str(p)}) for p in files]
    requests += [("definition", {"symbolName": args.symbol})]
    results = {"rounds": [], "server": str(Path(args.server).resolve())}
    start = time.perf_counter()
    with MCPClient(args.server, root, args.lsp) as client:
        results["startup_ms"] = round((time.perf_counter() - start)*1000, 2)
        def call(request):
            name, arguments = request
            start = time.perf_counter()
            text = client.call(name, arguments)
            if name == "definition" and text.endswith("not found"):
                raise AssertionError(text)
            result = {"tool": name, "ms": round((time.perf_counter()-start)*1000, 2), "bytes": len(text.encode()), "result_hash": hashlib.sha256(text.encode()).hexdigest()}
            if name == "diagnostics":
                count = re.search(r"Diagnostics in File: (\d+)",text)
                result["diagnostics"] = int(count[1]) if count else 0
            return result
        with ThreadPoolExecutor(max_workers=len(requests)) as pool:
            for _ in range(args.rounds):
                start = time.perf_counter()
                responses = list(pool.map(call, requests))
                if args.expect_diagnostics is not None:
                    counts = [r["diagnostics"] for r in responses if r["tool"] == "diagnostics"]
                    assert counts == args.expect_diagnostics, f"diagnostics changed: expected {args.expect_diagnostics}, got {counts}"
                results["rounds"].append({"batch_ms": round((time.perf_counter()-start)*1000, 2), "responses": responses})
    after = hashes()
    results["inputs_unchanged"] = before == after
    results["cold_batch_ms"] = results["rounds"][0]["batch_ms"]
    if len(results["rounds"]) > 1:
        warm = [r["batch_ms"] for r in results["rounds"][1:]]
        results["warm_batch_median_ms"] = round(statistics.median(warm), 2)
        results["warm_batch_max_ms"] = max(warm)
    print(json.dumps(results, indent=2))
    assert results["inputs_unchanged"], "project files/config changed during benchmark; rerun for comparable results"


if __name__ == "__main__":
    main()
