"""Small stdlib-only MCP stdio client for reproducible checks and benchmarks."""

import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import tempfile
import threading


class MCPClient:
    def __init__(self, server, workspace, lsp):
        self.log = tempfile.TemporaryFile(mode="w+")
        self.process = subprocess.Popen(
            [str(Path(server).resolve()), "--workspace", str(Path(workspace).resolve()),
             "--lsp", str(Path(lsp).resolve()), "--", "--stdio"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log,
            text=True, start_new_session=True,
        )
        self.lock = threading.Lock()
        self.pending = {}
        self.next_id = 0
        self.closed = False
        self.forced_shutdown = False
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    def _read(self):
        try:
            for line in self.process.stdout:
                message = json.loads(line)
                with self.lock:
                    result = self.pending.get(message.get("id"))
                if result is not None:
                    result.put(message)
        except Exception as error:
            self._fail_pending(error)
        finally:
            self._fail_pending(RuntimeError("MCP server closed stdout"))

    def _fail_pending(self, error):
        with self.lock:
            for result in self.pending.values():
                result.put(error)

    def request(self, method, params=None, *, notification=False, timeout=65):
        with self.lock:
            self.next_id += 1
            request_id = self.next_id
            result = queue.Queue()
            message = {"jsonrpc": "2.0", "method": method, "params": params or {}}
            if not notification:
                message["id"] = request_id
                self.pending[request_id] = result
            self.process.stdin.write(json.dumps(message) + "\n")
            self.process.stdin.flush()
        if notification:
            return None
        try:
            response = result.get(timeout=timeout)
            if isinstance(response, Exception):
                raise response
            if "error" in response:
                raise RuntimeError(response["error"])
            return response["result"]
        except queue.Empty as error:
            self.request("notifications/cancelled", {"requestId": request_id}, notification=True)
            raise TimeoutError(f"MCP request timed out: {method}") from error
        finally:
            with self.lock:
                self.pending.pop(request_id, None)

    def call(self, tool, arguments):
        result = self.request("tools/call", {"name": tool, "arguments": arguments})
        text = "\n".join(item.get("text", "") for item in result.get("content", []))
        if result.get("isError"):
            raise RuntimeError(text)
        return text

    def __enter__(self):
        try:
            self.request("initialize", {
                "protocolVersion": "2024-11-05", "capabilities": {},
                "clientInfo": {"name": "bridge-check", "version": "1"},
            })
            self.request("notifications/initialized", notification=True)
            return self
        except BaseException:
            self.close()
            raise

    def close(self):
        if self.closed:
            return
        self.closed = True
        self.process.stdin.close()
        try:
            self.process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            self.forced_shutdown = True
            os.killpg(self.process.pid, signal.SIGTERM)
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait()
        self.reader.join(timeout=2)
        self.log.seek(0)
        self.stderr_text = self.log.read()
        self.log.close()

    def __exit__(self, kind, value, traceback):
        self.close()
        if kind is None:
            if "WARNING: DATA RACE" in self.stderr_text:
                raise AssertionError(self.stderr_text)
            if not self.forced_shutdown and self.process.returncode:
                raise RuntimeError(f"MCP server exited {self.process.returncode}: {self.stderr_text[-2000:]}")
