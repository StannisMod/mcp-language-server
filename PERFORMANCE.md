# Performance and correctness

This fork optimizes the Pyright MCP path without changing project type-checking
rules or replacing the project's language server. It is derived from
[`isaacphi/mcp-language-server`](https://github.com/isaacphi/mcp-language-server),
with the original BSD-3-Clause license and attribution retained.

## What changed

- **Eight concurrent read workers**, with a bounded queue. File edits and renames
  are exclusive with respect to other tool calls. Initialization, cancellation,
  and control messages do not wait behind slow tool calls.
- **Atomic LSP writes.** Headers, bodies, and related document-refresh messages
  cannot interleave. LSP response waits honor cancellation, connection closure,
  and a 60-second deadline.
- **No fixed diagnostic/startup sleeps on the Pyright path.** Startup waits for
  the watcher to actually become ready. Diagnostics wait for versioned pushes and
  completed analysis, rather than sleeping three seconds or using the unsupported
  `textDocument/diagnostic` request in Pyright 1.1.390.
- **No forced opening of watched files for Pyright.** Pyright indexes its own
  workspace and honors its own include/exclude settings. Existing and newly
  generated artifacts are not silently opened as editor buffers.
- **Immediate file-change forwarding for Pyright.** The bridge does not add a
  second 300ms debounce on top of Pyright's own scheduling. Open documents are
  also checked against disk before diagnostics, including open dependencies.
- **Cheap refresh for unchanged documents.** An atomic close/reopen requests a
  new versioned publication while retaining Pyright's existing analysis where
  possible. A no-op `didChange` would force Pyright to invalidate its evaluator
  and recheck importers. Actual content changes still use `didChange`.
- **Qualified Python definition fast path.** For a module located directly in
  the workspace, a qualified name uses document symbols instead of a workspace
  scan. Nonstandard source layouts retain workspace search. Symbols are reused
  only within that one request, not cached across edits.
- **Less redundant formatting work.** Context rendering fetches document symbols
  once per file, not once per diagnostic/reference. The advertised boolean
  `contextLines` option now works: false/default returns compact diagnostics;
  true includes surrounding code. All diagnostics, messages, and codes remain.
- **Clean shutdown.** Stdio EOF cancels pending work, stops the watcher, and
  shuts down the language server. Cleanup is idempotent.

The automatic behavior above recognizes Pyright-family executable names containing
`pyright`. Other language servers retain their existing diagnostic retrieval path;
their versioned-push behavior has not been assumed to match Pyright's. The legacy
`LSP_MCP_SKIP_INITIAL_OPEN=1` override is also supported for self-indexing wrappers.

## Why a version number is not enough

There are two protocols:

```text
MCP client -> bridge: diagnostics(file)       request
bridge -> Pyright: document notifications    LSP notifications
Pyright -> bridge: diagnostic publications   LSP notifications
bridge -> MCP client: completed diagnostics  one response
```

On a large file, Pyright can first publish an **interim empty** diagnostic list,
then begin/report analysis progress, and finally publish errors **at the same
document version**. Returning the first matching version would incorrectly report
that the file is clean.

The bridge therefore tracks diagnostic versions and Pyright analysis progress.
A standard hover request/response acts as an ordering barrier, so a waiter cannot
mistake the gap between an interim publication and the following `beginProgress`
for completed analysis. A result is returned only after the corresponding
version is available and analysis is idle. Disk changes detected during the wait
cause synchronization and another wait. Missing results and timeouts are errors,
never an empty/clean result.

There is deliberately **no TTL-based diagnostic-result cache**. An unchanged target
file can acquire errors when one of its dependencies changes.

## Measured example

Local Linux x86-64 workspace, project Pyright 1.1.390. The batch contains three
diagnostics calls and one qualified definition call, sent concurrently. Source
and configuration hashes were checked; the three files consistently produced
19, 0, and 0 diagnostics, including the first cold request.

| Measurement | Earlier fixed bridge | Optimized bridge |
| --- | ---: | ---: |
| Startup | about 1.14s | about 0.33s |
| Cold batch | about 9.78s | about 5.30s |
| Warm batch median | about 9.85s | about 0.265s |

This run used Go 1.24.13; the optimized warm batch maximum was 0.267s over six
warm rounds. Its qualified definition call had a median latency of about 27ms.
Both runs checked expected diagnostic counts on every round, not just timings.

These are local measurements, not latency guarantees. Cold analysis and actual
source/dependency changes still cost real Pyright work. A warm unchanged batch
does not need to rebuild that analysis. Reproduce on your own workspace rather
than comparing different files or disabling type-checking rules.

## Checks

Unit tests, including frame interleaving, stale/late/empty diagnostics, progress
ordering, cancellation, concurrent opens, MCP dispatch, watcher behavior, and
compact context options:

```sh
go test -race ./internal/... .
go vet ./...
```

The original Python integration/snapshot suite was also run under the race
detector, without updating its snapshots:

```sh
go test -race ./integrationtests/tests/python/...
```

The real stdio MCP path is checked with both Pyright 1.1.390 and 1.1.414. The
stdlib-only Python script writes only to a temporary workspace. It covers a cold
large-file check, immediate edits without sleeps, open and closed dependency
changes, qualified definitions, nonstandard source layouts, 16 concurrent checks,
MCP edit/rename, missing-file failure, and graceful shutdown. A race-built server
is supported and race reports fail the check:

```sh
go build -race -o mcp-language-server .
python3 scripts/python_smoke.py --server ./mcp-language-server --lsp /absolute/path/to/pyright-langserver
```

Read-only benchmarks require an existing workspace. An optional expected count
for each file is checked on **every** cold/warm round to catch false speedups
from missing diagnostics. Benchmark JSON also reports input-hash stability:

```sh
go build -o mcp-language-server .
python3 scripts/benchmark.py --server ./mcp-language-server --lsp /absolute/path/to/pyright-langserver --workspace /project --file package/a.py --file package/b.py --symbol package.a.Example --rounds 7 --expect-diagnostics 1 0
```

The smoke/benchmark process-management scripts are tested on Linux. Other
language servers' full integration suites still need their respective binaries;
Python checks do not establish compatibility with every LSP implementation.
