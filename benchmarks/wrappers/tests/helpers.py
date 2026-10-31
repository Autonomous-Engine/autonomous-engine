"""Test fixtures: temp git repos, an in-process fake Autonomous Engine, a fake claude executable.

No network beyond 127.0.0.1, no Docker, standard library only.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse

BENCHMARKS_DIR = Path(__file__).resolve().parents[2]
if str(BENCHMARKS_DIR) not in sys.path:
    sys.path.insert(0, str(BENCHMARKS_DIR))

REPO_ROOT = BENCHMARKS_DIR.parent

GIT_ID = ["-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "-c", "commit.gpgsign=false"]


def git(repo: Path, *args: str) -> str:
    proc = subprocess.run(["git", "-C", str(repo), *GIT_ID, *args], text=True, capture_output=True, check=False, encoding="utf-8")
    if proc.returncode != 0:
        raise AssertionError(f"git {' '.join(args)} failed: {proc.stderr.strip()}")
    return proc.stdout.strip()


def make_repo(root: Path) -> dict[str, Any]:
    """source -> remote.git (bare) -> bench (BENCH_REPO clone whose origin is the bare)."""
    source = root / "source"
    source.mkdir()
    git(source, "init", "-q", "-b", "main")
    (source / "README.md").write_text("fixture\n", encoding="utf-8")
    (source / "app.py").write_text("def add(a, b):\n    return a + b\n", encoding="utf-8")
    git(source, "add", "-A")
    git(source, "commit", "-q", "-m", "base")
    base_sha = git(source, "rev-parse", "HEAD")

    remote = root / "remote.git"
    subprocess.run(["git", "clone", "-q", "--bare", str(source), str(remote)], check=True)
    bench = root / "bench"
    subprocess.run(["git", "clone", "-q", "--no-hardlinks", str(remote), str(bench)], check=True)
    git(bench, "checkout", "-q", base_sha)  # detached, like the runner
    # Sealed fixture clones have no origin the product can push to; make sure
    # the wrappers never lean on one.
    git(bench, "remote", "remove", "origin")
    return {"source": source, "remote": remote, "bench": bench, "base_sha": base_sha}


def push_agent_branch(root: Path, remote: Path, branch: str, files: dict[str, str], message: str = "agent change") -> str:
    """Simulate the implementation-agent: branch from main, commit files, push to the remote."""
    work = root / ("work-" + uuid.uuid4().hex[:8])
    subprocess.run(["git", "clone", "-q", str(remote), str(work)], check=True)
    git(work, "checkout", "-q", "-b", branch, "origin/main")
    for rel, content in files.items():
        path = work / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")
    git(work, "add", "-A")
    git(work, "commit", "-q", "-m", message)
    sha = git(work, "rev-parse", "HEAD")
    git(work, "push", "-q", "origin", branch)
    shutil.rmtree(work, ignore_errors=True)
    return sha


# --- fake Autonomous Engine (gateway + dashboard-backend on one port) ---------

GATES = ("prd_approval", "human_review")
GATE_ENDPOINTS = {"/api/work-items/approve-prd": "prd_approval", "/api/work-items/approve": "human_review"}


class FakeAE:
    """Scripted work-item lifecycle. Each detail poll advances one state unless parked at a human gate."""

    def __init__(self, script: list[str], token: str = "test-token", reasons: dict[str, str] | None = None, repository_id: str | None = None, stateless: bool = False, prompts: dict[str, dict[str, Any]] | None = None):
        self.script = list(script)
        self.token = token
        self.reasons = reasons or {}
        self.repository_id = repository_id
        self.stateless = stateless  # serve HTTP 200 detail bodies without item.state
        self.cost_error = False  # make /api/work-items/cost answer HTTP 500 throughout
        # awaiting_user prompts keyed by the script index they belong to (the
        # product's interaction surface): {index: {"trigger":..., "actions":[...]}}.
        # An awaiting_user state with no prompt configured is parked forever.
        self.prompts = prompts or {}
        self.answered_prompts: list[dict[str, Any]] = []
        self.answered_prompt_ids: set[str] = set()
        self.index = 0
        self.work_id = str(uuid.uuid4())
        self.product_id = str(uuid.uuid4())
        self.requests: list[dict[str, Any]] = []
        self.approvals: list[str] = []
        self.approved_gates: set[str] = set()
        self.cancelled = False
        self.intake_body: dict[str, Any] | None = None
        self.reject_intake = False
        self.pending: list[dict[str, Any]] = []
        self.transitions: list[dict[str, Any]] = []
        self.cost = {"total_input_tokens": 1234, "total_output_tokens": 567, "total_tokens": 1801, "total_llm_calls": 9}
        self.lock = threading.Lock()

    def current_state(self) -> str:
        return self.script[min(self.index, len(self.script) - 1)]

    def _advance(self) -> None:
        if self.index < len(self.script) - 1:
            old = self.current_state()
            self.index += 1
            new = self.current_state()
            self.transitions.append({"from_state": old, "to_state": new, "reason": self.reasons.get(new, ""), "agent_id": "fake"})

    def detail_payload(self) -> dict[str, Any]:
        state = self.current_state()
        if self.stateless:
            return {"item": {"id": self.work_id}, "transitions": [], "artifacts": []}
        payload = {
            "item": {"id": self.work_id, "state": state, "product_id": self.product_id, "repository_id": self.repository_id},
            "transitions": list(self.transitions),
            "agent_definition": {"name": "fake-def", "provider": "fake", "model_id": "fake-model-1"},
            "inference_logs": [{"model": "fake/fake-model-1", "input_tokens": 1, "output_tokens": 1}],
        }
        if state in GATES and state not in self.approved_gates:
            return payload  # parked until approved
        if state == "awaiting_user" and self._prompt_id(self.index) not in self.answered_prompt_ids:
            return payload  # parked until the pending prompt is answered
        self._advance()
        return payload

    def _prompt_id(self, index: int) -> str:
        return f"prompt-{index}"

    def pending_prompt_rows(self) -> list[dict[str, Any]]:
        """GET /api/prompts: the pending prompt for the current awaiting_user state, if any."""
        if self.current_state() != "awaiting_user":
            return []
        spec = self.prompts.get(self.index)
        pid = self._prompt_id(self.index)
        if spec is None or pid in self.answered_prompt_ids:
            return []
        row = {"id": pid, "work_item_id": self.work_id, "product_id": self.product_id, "state": "pending", "headline": spec.get("headline", "fake prompt")}
        row.update({k: v for k, v in spec.items() if k != "headline"})
        return [row]

    def answer_prompt(self, prompt_id: str, action: str) -> int:
        if prompt_id != self._prompt_id(self.index) or self.current_state() != "awaiting_user" or prompt_id in self.answered_prompt_ids:
            return 409
        spec = self.prompts.get(self.index) or {}
        if action not in {a.get("id") for a in spec.get("actions", [])}:
            return 400
        self.answered_prompt_ids.add(prompt_id)
        self.answered_prompts.append({"id": prompt_id, "action": action, "trigger": spec.get("trigger")})
        self._advance()
        return 200

    def approve(self, gate: str) -> int:
        if self.current_state() != gate:
            return 409
        self.approved_gates.add(gate)
        self.approvals.append(gate)
        self._advance()
        return 200

    def cancel(self) -> None:
        self.cancelled = True
        self.script = self.script[: self.index + 1] + ["cancelled"]
        self._advance()


def _handler_for(state: FakeAE) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args: Any) -> None:  # silence
            pass

        def _send(self, status: int, body: Any) -> None:
            data = json.dumps(body).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _authed(self) -> bool:
            return self.headers.get("Authorization") == "Bearer " + state.token

        def _record(self, body: Any = None) -> None:
            state.requests.append({"method": self.command, "path": self.path, "body": body, "auth": self.headers.get("Authorization")})

        def do_GET(self) -> None:  # noqa: N802
            url = urlparse(self.path)
            with state.lock:
                self._record()
                if url.path == "/api/version":
                    return self._send(200, {"version": "fake-1.0"})
                if not self._authed():
                    return self._send(401, "unauthorized")
                if url.path == "/api/work-items/detail":
                    wid = parse_qs(url.query).get("id", [""])[0]
                    if wid != state.work_id:
                        return self._send(404, {"error": "work item not found"})
                    return self._send(200, state.detail_payload())
                if url.path == "/api/work-items/cost":
                    if state.cost_error:
                        return self._send(500, {"error": "internal server error"})
                    return self._send(200, {"work_item_id": state.work_id, **state.cost})
                if url.path == "/api/prompts":
                    return self._send(200, state.pending_prompt_rows())
                if url.path == "/api/work-items/pending-changes":
                    return self._send(200, state.pending)
                return self._send(404, {"error": "not found"})

        def do_POST(self) -> None:  # noqa: N802
            url = urlparse(self.path)
            length = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(length) if length else b""
            try:
                body = json.loads(raw.decode("utf-8")) if raw else {}
            except json.JSONDecodeError:
                body = {"_raw": raw.decode("utf-8", "replace")}
            with state.lock:
                self._record(body)
                if url.path == "/api/intake":
                    if state.reject_intake:
                        return self._send(503, {"error": "Product has not been onboarded.", "code": "not_onboarded"})
                    if not body.get("description"):
                        return self._send(400, "description is required")
                    state.intake_body = body
                    return self._send(201, {"work_id": state.work_id, "product_id": state.product_id, "status": "accepted"})
                if not self._authed():
                    return self._send(401, "unauthorized")
                if url.path in GATE_ENDPOINTS:
                    code = state.approve(GATE_ENDPOINTS[url.path])
                    if code == 200:
                        return self._send(200, {"status": "approved", "work_item_id": state.work_id})
                    return self._send(code, {"error": f"work item is in state {state.current_state()!r}"})
                if url.path == "/api/work-items/cancel":
                    state.cancel()
                    return self._send(200, {"status": "cancelled"})
                if url.path.startswith("/api/prompts/") and url.path.endswith("/answer"):
                    pid = url.path[len("/api/prompts/"):-len("/answer")]
                    code = state.answer_prompt(pid, str(body.get("action_chosen", "")))
                    if code == 200:
                        return self._send(200, {"status": "answered", "id": pid})
                    return self._send(code, {"error": f"prompt {pid} not answerable in state {state.current_state()!r}"})
                return self._send(404, {"error": "not found"})

    return Handler


def start_fake_ae(state: FakeAE) -> tuple[ThreadingHTTPServer, str]:
    server = ThreadingHTTPServer(("127.0.0.1", 0), _handler_for(state))
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, f"http://127.0.0.1:{server.server_address[1]}"


# --- fake claude executable ----------------------------------------------------

FAKE_CLAUDE_PY = r'''
import json, os, sys, time
args = sys.argv[1:]
if "--version" in args:
    print("0.0.0-fake (Claude Code)")
    sys.exit(0)
mode = os.environ.get("FAKE_CLAUDE_MODE", "success")
prompt = sys.stdin.read()
argfile = os.environ.get("FAKE_CLAUDE_ARGS_FILE")
if argfile:
    with open(argfile, "w", encoding="utf-8") as fh:
        json.dump({"argv": args, "prompt": prompt, "cwd": os.getcwd()}, fh)
if mode == "hang":
    with open("half-done.txt", "w", encoding="utf-8") as fh:
        fh.write("partial\n")
    pidfile = os.environ.get("FAKE_CLAUDE_PID_FILE")
    if pidfile:
        with open(pidfile, "w", encoding="utf-8") as fh:
            fh.write(str(os.getpid()))
    time.sleep(120)
    sys.exit(0)
if mode == "fail":
    print(json.dumps({"type": "result", "subtype": "error_during_execution", "is_error": True, "result": "boom",
                      "num_turns": 2, "total_cost_usd": 0.01, "usage": {"input_tokens": 5, "output_tokens": 3}}))
    sys.exit(7)
if mode == "orphan":
    # Claude "succeeds" but leaves HEAD on an orphan commit that does not contain the base.
    import subprocess as sp
    ident = ["-c", "user.name=claude", "-c", "user.email=claude@localhost"]
    sp.run(["git", *ident, "checkout", "-q", "--orphan", "scratch"], check=True)
    with open("rewrite.txt", "w", encoding="utf-8") as fh:
        fh.write("rewritten\n")
    sp.run(["git", *ident, "add", "-A"], check=True)
    sp.run(["git", *ident, "commit", "-q", "-m", "orphan rewrite"], check=True)
    print(json.dumps({"type": "result", "subtype": "success", "is_error": False, "result": "Done", "num_turns": 1,
                      "total_cost_usd": 0.01, "usage": {"input_tokens": 5, "output_tokens": 3}}))
    sys.exit(0)
if mode == "nuke_git":
    # Claude "succeeds" but destroys the repository so nothing can be committed.
    import shutil, stat
    def _rw(func, path, _exc):
        os.chmod(path, stat.S_IWRITE)
        func(path)
    shutil.rmtree(".git", onexc=_rw)
    print(json.dumps({"type": "result", "subtype": "success", "is_error": False, "result": "Done", "num_turns": 1,
                      "total_cost_usd": 0.01, "usage": {"input_tokens": 5, "output_tokens": 3}}))
    sys.exit(0)
if mode == "error_exit0":
    print(json.dumps({"type": "result", "subtype": "error_max_budget_usd", "is_error": True, "result": "budget",
                      "num_turns": 1, "total_cost_usd": 0.5, "usage": {"input_tokens": 5, "output_tokens": 3}}))
    sys.exit(0)
with open("claude-output.txt", "w", encoding="utf-8") as fh:
    fh.write("hello from fake claude\n")
print(json.dumps({
    "type": "result", "subtype": "success", "is_error": False, "result": "Done", "num_turns": 3,
    "total_cost_usd": 0.1234, "session_id": "fake-session", "duration_ms": 10, "permission_denials": [],
    "modelUsage": {"claude-fake-1": {"inputTokens": 10}},
    "usage": {"input_tokens": 10, "cache_creation_input_tokens": 100, "cache_read_input_tokens": 1000,
              "output_tokens": 42, "iterations": [{}, {}, {}]},
}))
sys.exit(0)
'''


def make_fake_claude(bin_dir: Path) -> Path:
    """Write a fake ``claude`` into ``bin_dir`` (``claude.cmd`` on Windows). Returns the script path."""
    bin_dir.mkdir(parents=True, exist_ok=True)
    script = bin_dir / "fake_claude.py"
    script.write_text(FAKE_CLAUDE_PY, encoding="utf-8")
    python = sys.executable
    if os.name == "nt":
        shim = bin_dir / "claude.cmd"
        shim.write_text(f'@echo off\r\n"{python}" "{script}" %*\r\n', encoding="utf-8")
    else:
        shim = bin_dir / "claude"
        shim.write_text(f'#!/bin/sh\nexec "{python}" "{script}" "$@"\n', encoding="utf-8")
        shim.chmod(0o755)
    return shim


def path_with(bin_dir: Path, environ: dict[str, str]) -> str:
    return str(bin_dir) + os.pathsep + environ.get("PATH", "")


def process_alive(pid: int) -> bool:
    """True if a process with ``pid`` still exists (never sends a signal on Windows)."""
    if os.name == "nt":
        out = subprocess.run(["tasklist", "/FI", f"PID eq {pid}", "/NH"], capture_output=True, text=True, check=False).stdout
        return str(pid) in out
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def wait_for_death(pid: int, timeout_s: float = 10.0) -> bool:
    """Poll until the process is gone; returns True when it is dead."""
    import time

    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if not process_alive(pid):
            return True
        time.sleep(0.2)
    return not process_alive(pid)
