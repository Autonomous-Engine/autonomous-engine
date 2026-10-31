"""Autonomous Engine competitor wrapper.

Flow (see README.md for the env vars):

1. ``POST {AE_GATEWAY_URL}/api/intake`` with the prompt as ``description``
   (``cmd/gateway/main.go``: the only required field; ``product_id`` is
   optional when exactly one product exists; ``repository_id`` cannot be set
   through intake - the gateway auto-assigns it for single-repo products, so
   ``AE_REPOSITORY_ID`` is sent as metadata and cross-checked on the polled
   work item).
2. Poll ``GET {AE_DASHBOARD_URL}/api/work-items/detail?id=<work_id>`` (JWT
   bearer, ``cmd/dashboard-backend``) until a terminal state or the deadline.
   The product parks its human gates (PRD approval, human review, PR gate)
   in ``awaiting_user`` with a pending ``approval:*`` prompt from the unified
   interaction surface; with ``AE_AUTO_APPROVE=1`` (default) the wrapper
   answers the prompt's ``continue`` action through ``POST
   /api/prompts/{id}/answer``, the same call the dashboard makes when a human
   clicks Approve, so an unattended benchmark can finish. The legacy
   ``prd_approval``/``human_review`` states are approved through their
   dashboard endpoints. An ``awaiting_user`` item whose pending prompt is not
   an approval (the product asked a question) is reported as ``blocked`` -
   there is nobody to answer.
3. On success discover the produced branches: the implementation-agent pushes
   ``agent/<work_id>/<task8>`` to the repository's remote
   (``cmd/implementation-agent/main.go``); ``git ls-remote`` on that prefix
   against ``AE_FETCH_REMOTE`` (a URL/path, never ``origin`` - sealed fixture
   clones have no usable origin) is the source of truth.
   ``/api/work-items/pending-changes`` rows are recorded as a cross-check when
   present. Fetch, check out (merging when the product produced several
   subtask branches), verify the result contains the base revision, and
   report ``HEAD`` as ``head_sha``.
4. Token counts come from ``GET /api/work-items/cost?id=`` (aggregated
   ``inference_logs`` tagged with the work item or its subtasks). Services
   built before M4-05 tag only some calls, so the numbers stay labelled a
   LOWER BOUND (``details.token_accounting = "partial"``); a terminal run
   with ``total_llm_calls == 0`` is reported as unknown (null), not zero.
   ``cost_usd`` is the endpoint's priced cost (``model_prices``) when every
   call with tokens has a price (``cost_unknown`` false) and null otherwise
   - never 0 for unknown. When ``BENCH_MAX_TOKENS`` is set the same endpoint is
   polled during the run; a breach cancels the work item and scores
   ``failed``, and a cap that cannot be verified by run end also scores
   ``failed`` (fail closed, like the Claude wrapper).

Exit codes: 0 passed_agent, 1 failed, 2 blocked, 3 timeout, 4 usage error.
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Mapping

from . import common
from .common import (
    EXIT_USAGE,
    OUTCOME_BLOCKED,
    OUTCOME_FAILED,
    OUTCOME_PASSED,
    OUTCOME_TIMEOUT,
    BenchEnv,
    Deadline,
    GitError,
    RawLog,
    Result,
    WrapperError,
    git,
)

COMPETITOR = "autonomous-engine"

DEFAULT_GATEWAY_URL = "http://localhost:8082"
DEFAULT_DASHBOARD_URL = "http://localhost:8080"
DEFAULT_POLL_INTERVAL_S = 10.0
DEFAULT_SOURCE = "benchmark"
MAX_CONSECUTIVE_POLL_FAILURES = 5
HTTP_TIMEOUT_S = 30.0
# Git work after the polling phase (ls-remote, fetch, checkout, merge, diff) is
# bounded by the remaining time budget, floored so a run that finishes right at
# the deadline can still fetch, and capped so it can never overshoot by minutes.
TAIL_GIT_MIN_S = 30.0
TAIL_GIT_MAX_S = 300.0

# internal/models/workitem.go states, classified for the benchmark.
SUCCESS_STATES = frozenset({"passed", "released"})
FAILED_STATES = frozenset({"failed", "rejected", "cancelled"})
BLOCKED_STATES = frozenset({"blocked", "awaiting_user"})
TERMINAL_STATES = SUCCESS_STATES | FAILED_STATES | BLOCKED_STATES
# Human gates -> dashboard endpoint that a human would click.
GATE_ENDPOINTS = {
    "prd_approval": "/api/work-items/approve-prd",
    "human_review": "/api/work-items/approve",
}
# The product's unified interaction surface parks the same human gates in
# ``awaiting_user`` with a pending prompt (internal/interactions: trigger
# ``approval:*``, one action per button). Those prompts are answered through
# ``POST /api/prompts/{id}/answer``; any other pending prompt (a clarification,
# an escalation, a block question) is a genuine blocker.
PROMPT_APPROVAL_TRIGGERS = frozenset({"approval:prd", "approval:human_review", "approval:pr_gate"})
# ``awaiting_user`` can be observed a poll or two before the prompt row exists,
# and stays for a poll after the answer while the orchestrator transitions.
AWAITING_USER_GRACE_POLLS = 3
# Every other state (intake, prd, research, planning, implementation, review,
# qa, and any state added later) is treated as "in progress".

# Non-nested on purpose: the runner's own checkout creates a local branch named
# `bench`, and `bench/...` would be a ref directory/file conflict with it.
BENCH_BRANCH = "bench-autonomous-engine"
MAX_MALFORMED_POLLS = 5
FETCH_REMOTE_HELP = (
    "AE_FETCH_REMOTE is required: paired AE runs need it to point at the fork the product pushes "
    "agent/<work_id>/* branches to (a fork of the fixture repository at the same base revision). "
    "The wrapper never uses the clone's origin, because sealed fixture clones have none the product can push to."
)


class AEAPIError(RuntimeError):
    def __init__(self, message: str, status: int | None = None, body: str = ""):
        super().__init__(message)
        self.status = status
        self.body = body


@dataclasses.dataclass
class AEConfig:
    gateway_url: str
    dashboard_url: str
    api_token: str
    product_id: str | None
    repository_id: str | None
    poll_interval_s: float
    fetch_remote: str
    auto_approve: bool
    source: str
    tags: list[str]
    complexity: str
    # M4-06 shadow evaluation: the configuration snapshot the intake asks the
    # gateway to run (honoured for benchmark-sourced intakes only) and the
    # rollout the run belongs to. Empty = route normally.
    config_snapshot_id: str = ""
    rollout_id: str = ""

    @classmethod
    def from_environ(cls, environ: Mapping[str, str] | None = None) -> "AEConfig":
        environ = os.environ if environ is None else environ
        gateway = environ.get("AE_GATEWAY_URL", "").strip() or DEFAULT_GATEWAY_URL
        dashboard = environ.get("AE_DASHBOARD_URL", "").strip() or DEFAULT_DASHBOARD_URL
        token = environ.get("AE_API_TOKEN", "").strip()
        if not token:
            raise WrapperError(
                "AE_API_TOKEN is required (dashboard-backend JWT from POST /api/auth/login; "
                "the gateway itself is unauthenticated but polling and approvals are not)"
            )
        fetch_remote = environ.get("AE_FETCH_REMOTE", "").strip()
        if not fetch_remote:
            raise WrapperError(FETCH_REMOTE_HELP)
        tags_raw = environ.get("AE_TAGS", "").strip()
        tags = [t.strip() for t in tags_raw.split(",") if t.strip()] if tags_raw else []
        return cls(
            gateway_url=gateway.rstrip("/"),
            dashboard_url=dashboard.rstrip("/"),
            api_token=token,
            product_id=environ.get("AE_PRODUCT_ID", "").strip() or None,
            repository_id=environ.get("AE_REPOSITORY_ID", "").strip() or None,
            poll_interval_s=common.float_env(environ, "AE_POLL_INTERVAL_S", DEFAULT_POLL_INTERVAL_S),
            fetch_remote=fetch_remote,
            auto_approve=common.bool_env(environ, "AE_AUTO_APPROVE", True),
            source=environ.get("AE_SOURCE", "").strip() or DEFAULT_SOURCE,
            tags=tags,
            complexity=environ.get("AE_COMPLEXITY", "").strip(),
            config_snapshot_id=environ.get("AE_CONFIG_SNAPSHOT_ID", "").strip(),
            rollout_id=environ.get("AE_ROLLOUT_ID", "").strip(),
        )

    def public(self) -> dict[str, Any]:
        d = dataclasses.asdict(self)
        d["api_token"] = "<redacted>" if self.api_token else ""
        return d


class AEClient:
    """Thin urllib client for the gateway (intake) and dashboard-backend (everything else)."""

    def __init__(self, cfg: AEConfig, log: RawLog):
        self.cfg = cfg
        self.log = log

    def _request(self, base: str, path: str, method: str = "GET", body: Any = None, timeout: float = HTTP_TIMEOUT_S) -> Any:
        url = base + path
        data = None
        headers = {"Accept": "application/json", "Authorization": "Bearer " + self.cfg.api_token}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=max(1.0, timeout)) as resp:
                raw = resp.read().decode("utf-8", errors="replace")
                status = resp.status
        except urllib.error.HTTPError as exc:
            raw = exc.read().decode("utf-8", errors="replace") if exc.fp else ""
            raise AEAPIError(f"{method} {url} -> HTTP {exc.code}: {raw.strip()[:500]}", status=exc.code, body=raw) from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise AEAPIError(f"{method} {url} -> {exc}") from exc
        if not raw.strip():
            return {}
        try:
            return json.loads(raw)
        except json.JSONDecodeError as exc:
            raise AEAPIError(f"{method} {url} -> HTTP {status} with non-JSON body: {raw.strip()[:200]}", status=status, body=raw) from exc

    # gateway
    def submit(self, description: str, metadata: dict[str, Any]) -> str:
        body: dict[str, Any] = {
            "description": description,
            "source": self.cfg.source,
            "metadata": metadata,
            "tags": self.cfg.tags,
            "complexity": self.cfg.complexity,
        }
        if self.cfg.product_id:
            body["product_id"] = self.cfg.product_id
        resp = self._request(self.cfg.gateway_url, "/api/intake", "POST", body)
        work_id = str(resp.get("work_id", "")).strip() if isinstance(resp, dict) else ""
        if not work_id:
            raise AEAPIError(f"intake response has no work_id: {resp!r}")
        return work_id

    # dashboard-backend
    def detail(self, work_id: str, timeout: float = HTTP_TIMEOUT_S) -> dict[str, Any]:
        return self._request(self.cfg.dashboard_url, "/api/work-items/detail?id=" + urllib.parse.quote(work_id), timeout=timeout)

    def approve(self, work_id: str, endpoint: str) -> Any:
        return self._request(self.cfg.dashboard_url, endpoint + "?id=" + urllib.parse.quote(work_id), "POST", {})

    def pending_prompts(self, product_id: str | None) -> list[dict[str, Any]]:
        path = "/api/prompts"
        if product_id:
            path += "?product_id=" + urllib.parse.quote(product_id)
        rows = self._request(self.cfg.dashboard_url, path)
        return rows if isinstance(rows, list) else []

    def answer_prompt(self, prompt_id: str, action: str, text: str = "") -> Any:
        return self._request(
            self.cfg.dashboard_url,
            "/api/prompts/" + urllib.parse.quote(prompt_id) + "/answer",
            "POST",
            {"action_chosen": action, "answer_text": text},
        )

    def cancel(self, work_id: str) -> Any:
        return self._request(self.cfg.dashboard_url, "/api/work-items/cancel?id=" + urllib.parse.quote(work_id), "POST", {})

    def cost(self, work_id: str) -> dict[str, Any]:
        return self._request(self.cfg.dashboard_url, "/api/work-items/cost?id=" + urllib.parse.quote(work_id))

    def pending_changes(self) -> list[dict[str, Any]]:
        rows = self._request(self.cfg.dashboard_url, "/api/work-items/pending-changes")
        return rows if isinstance(rows, list) else []

    def version(self) -> str | None:
        try:
            resp = self._request(self.cfg.dashboard_url, "/api/version", timeout=10)
        except AEAPIError as exc:
            self.log.log(f"version lookup failed (non-fatal): {exc}")
            return None
        return str(resp.get("version")) if isinstance(resp, dict) and resp.get("version") else None


# --- head discovery / checkout ------------------------------------------------


def list_remote_agent_branches(repo: Path, remote: str, work_id: str, timeout: float | None = TAIL_GIT_MAX_S) -> dict[str, str]:
    """Return {branch: sha} for refs/heads/agent/<work_id>/* on ``remote`` (a URL or path, not a remote name)."""
    out = git(repo, "ls-remote", "--heads", remote, f"refs/heads/agent/{work_id}/*", timeout=timeout)
    found: dict[str, str] = {}
    for line in out.splitlines():
        parts = line.split("\t")
        if len(parts) != 2:
            continue
        sha, ref = parts[0].strip(), parts[1].strip()
        if ref.startswith("refs/heads/"):
            found[ref[len("refs/heads/"):]] = sha
    return found


def checkout_produced_branches(repo: Path, remote: str, branches: list[str], log: RawLog, timeout: float | None = TAIL_GIT_MAX_S) -> tuple[str, list[dict[str, str]]]:
    """Fetch the agent branches, check out the oldest, merge the rest.

    Returns ``(head_sha, merged)`` where ``merged`` lists every branch and its
    remote head in merge order. When more than one branch is involved the
    resulting tree is a WRAPPER-SIDE merge that the product never QA'd as one
    tree (each subtask branch is its own PR in the product); callers must
    disclose that in the output (``details.wrapper_side_merge``).
    """
    for branch in branches:
        git(repo, "fetch", "--quiet", remote, f"+refs/heads/{branch}:refs/bench-ae/{branch}", timeout=timeout)
    # Merge in commit-time order so review-fix branches (which build on earlier
    # implementation branches) land after the branch they extend.
    dated = []
    for branch in branches:
        ts = git(repo, "log", "-1", "--format=%ct", f"refs/bench-ae/{branch}", timeout=timeout)
        dated.append((int(ts or 0), branch))
    ordered = [b for _, b in sorted(dated)]
    merged: list[dict[str, str]] = []
    first, rest = ordered[0], ordered[1:]
    git(repo, "checkout", "--quiet", "-B", BENCH_BRANCH, f"refs/bench-ae/{first}", timeout=timeout)
    merged.append({"branch": first, "head": git(repo, "rev-parse", f"refs/bench-ae/{first}", timeout=timeout)})
    log.log(f"checked out {first} ({merged[0]['head']}) as {BENCH_BRANCH}")
    for branch in rest:
        head = git(repo, "rev-parse", f"refs/bench-ae/{branch}", timeout=timeout)
        try:
            git(repo, *common.GIT_BENCH_IDENTITY, "merge", "--quiet", "--no-edit", f"refs/bench-ae/{branch}", timeout=timeout)
            merged.append({"branch": branch, "head": head})
            log.log(f"wrapper-side merge of {branch} ({head})")
        except GitError as exc:
            git(repo, "merge", "--abort", check=False, timeout=timeout)
            raise GitError(f"subtask branches conflict: could not merge {branch} onto {first}: {exc}") from exc
    return git(repo, "rev-parse", "HEAD", timeout=timeout), merged


# --- main flow ----------------------------------------------------------------


def _state_of(detail: dict[str, Any]) -> str:
    item = detail.get("item") if isinstance(detail, dict) else None
    if isinstance(item, dict):
        return str(item.get("state", "")).strip()
    return ""


def _continue_action(prompt: dict[str, Any]) -> str | None:
    """The action id a human would click to approve: semantic ``continue``, else id ``approve``."""
    actions = prompt.get("actions") or []
    if not isinstance(actions, list):
        return None
    for act in actions:
        if isinstance(act, dict) and act.get("semantic") == "continue" and act.get("id"):
            return str(act["id"])
    for act in actions:
        if isinstance(act, dict) and act.get("id") == "approve":
            return "approve"
    return None


def _auto_answer_prompts(client: "AEClient", product_id: str | None, work_id: str, answered: set[str], details: dict[str, Any], elapsed: float, log: Any) -> str:
    """Answer pending approval prompts for ``work_id``.

    Returns ``"answered"`` when at least one approval prompt was answered,
    ``"blocking"`` when a pending prompt exists that is not an approval (the
    product asked a question - an honest blocker), and ``"none"`` when no
    pending prompt for this work item is visible yet.
    """
    try:
        prompts = client.pending_prompts(product_id)
    except AEAPIError as exc:
        log.log(f"listing pending prompts failed (will retry next poll): {exc}")
        return "none"
    mine = [p for p in prompts if isinstance(p, dict) and str(p.get("work_item_id", "")) == work_id and p.get("state", "pending") == "pending"]
    if not mine:
        return "none"
    outcome = "none"
    for prompt in mine:
        pid = str(prompt.get("id", ""))
        trigger = str(prompt.get("trigger", ""))
        if not pid or pid in answered:
            continue
        action = _continue_action(prompt) if trigger in PROMPT_APPROVAL_TRIGGERS else None
        if action is None:
            details["blocking_prompt"] = {"id": pid, "trigger": trigger, "prompt_type": prompt.get("prompt_type"), "headline": prompt.get("headline")}
            log.log(f"work item parked on a non-approval prompt {pid} ({trigger}): {prompt.get('headline')!r}")
            return "blocking"
        try:
            client.answer_prompt(pid, action)
        except AEAPIError as exc:
            log.log(f"answering prompt {pid} ({trigger}) with {action!r} failed: {exc}")
            if exc.status == 409:
                answered.add(pid)  # already answered elsewhere
            continue
        answered.add(pid)
        details["approvals"].append({"state": "awaiting_user", "prompt_id": pid, "trigger": trigger, "action": action, "t": round(elapsed, 3)})
        log.log(f"auto-approved prompt {pid} ({trigger}) via action {action!r}")
        outcome = "answered"
    return outcome


def _last_reason(detail: dict[str, Any]) -> str:
    transitions = detail.get("transitions") if isinstance(detail, dict) else None
    if isinstance(transitions, list) and transitions:
        last = transitions[-1]
        if isinstance(last, dict):
            return str(last.get("reason", "") or "")
    return ""


def _sleep_bounded(seconds: float, deadline: Deadline) -> None:
    time.sleep(max(0.0, min(seconds, deadline.remaining())))


def run(environ: Mapping[str, str] | None = None) -> int:
    """Execute the wrapper. Returns the process exit code."""
    environ = os.environ if environ is None else environ
    env = BenchEnv.from_environ(COMPETITOR, environ)
    cfg = AEConfig.from_environ(environ)
    deadline = Deadline(env.time_limit_s)
    log = RawLog(env.raw_dir / "wrapper.log", echo=common.echo_logs(environ))
    details: dict[str, Any] = {
        "config": cfg.public(),
        "state_history": [],
        "approvals": [],
        "raw_dir": str(env.raw_dir),
    }
    result = Result(
        competitor=COMPETITOR,
        task_id=env.task_id,
        outcome=OUTCOME_FAILED,
        elapsed_s=0.0,
        raw_ref=str(env.raw_dir / "wrapper.log"),
        details=details,
    )

    base_sha: str | None = None
    client: AEClient | None = None
    work_id: str | None = None

    def cancel(key: str) -> None:
        """Best-effort cancel so the product stops spending; records the result under details[key]."""
        if client is None or work_id is None:
            return
        try:
            client.cancel(work_id)
            details[key] = True
            log.log(f"cancelled work item {work_id} ({key})")
        except AEAPIError as exc:
            details[key] = False
            log.log(f"cancel ({key}) failed: {exc}")

    def tail_timeout() -> float:
        return max(TAIL_GIT_MIN_S, min(TAIL_GIT_MAX_S, deadline.remaining()))

    def finish(outcome: str, reason: str | None = None) -> int:
        common.record_diff_vs_base(env, result, base_sha, log, timeout=tail_timeout())
        return common.finalize(env, result, deadline, log, outcome, reason)

    try:
        log.log(f"autonomous-engine wrapper start task={env.task_id} repo={env.repo} time_limit_s={env.time_limit_s} max_tokens={env.max_tokens}")
        log.log(f"config: {json.dumps(cfg.public(), sort_keys=True)}")
        prompt = env.read_prompt()
        base_sha = common.git_head(env.repo)
        details["base_sha"] = base_sha
        details["max_tokens"] = env.max_tokens
        details["token_accounting"] = "partial"  # see _apply_cost
        client = AEClient(cfg, log)
        result.versions = common.base_versions(
            {
                "autonomous_engine": client.version(),
                "gateway_url": cfg.gateway_url,
                "dashboard_url": cfg.dashboard_url,
            }
        )

        # 1. submit
        metadata = {
            "benchmark": {"task_id": env.task_id, "base_sha": base_sha, "wrapper": common.WRAPPER_VERSION},
            "repository_id": cfg.repository_id,
        }
        if cfg.config_snapshot_id:
            # M4-06: for a rollout's shadow run on a scratch product
            # (AE_ROLLOUT_ID, AE_PRODUCT_ID) the gateway routes the intake to
            # this snapshot's agent definition and records it as requested;
            # the rollout evaluation checks the run actually used it.
            metadata["config_snapshot_id"] = cfg.config_snapshot_id
            result.versions["requested_config_snapshot_id"] = cfg.config_snapshot_id
        if cfg.rollout_id:
            metadata["rollout_id"] = cfg.rollout_id
        try:
            work_id = client.submit(prompt, metadata)
        except AEAPIError as exc:
            details["http_status"] = exc.status
            return finish(OUTCOME_FAILED, f"intake failed: {exc}")
        details["work_item_id"] = work_id
        log.log(f"submitted work item {work_id}")

        # 2. poll
        state = ""
        last_detail: dict[str, Any] = {}
        consecutive_failures = 0  # transport / HTTP errors
        malformed_polls = 0  # HTTP 200 bodies without item.state
        approved: set[tuple[str, int]] = set()
        answered_prompts: set[str] = set()
        awaiting_polls = 0
        timed_out = False
        while True:
            if deadline.expired():
                timed_out = True
                break
            try:
                last_detail = client.detail(work_id, timeout=min(HTTP_TIMEOUT_S, max(1.0, deadline.remaining())))
                consecutive_failures = 0
            except AEAPIError as exc:
                consecutive_failures += 1
                log.log(f"poll failed ({consecutive_failures}/{MAX_CONSECUTIVE_POLL_FAILURES}): {exc}")
                if exc.status in (401, 403):
                    return finish(OUTCOME_FAILED, f"dashboard rejected AE_API_TOKEN: {exc}")
                if consecutive_failures >= MAX_CONSECUTIVE_POLL_FAILURES:
                    return finish(OUTCOME_FAILED, f"dashboard unreachable: {exc}")
                _sleep_bounded(cfg.poll_interval_s, deadline)
                continue

            new_state = _state_of(last_detail)
            if not new_state:
                malformed_polls += 1
                log.log(f"poll returned no item.state ({malformed_polls}/{MAX_MALFORMED_POLLS}): {json.dumps(last_detail, default=str)[:300]}")
                if malformed_polls >= MAX_MALFORMED_POLLS:
                    details["malformed_polls"] = malformed_polls
                    cancel("cancelled_after_malformed_response")
                    return finish(OUTCOME_FAILED, f"dashboard returned {malformed_polls} consecutive work-item payloads without item.state; cannot track the run")
                _sleep_bounded(cfg.poll_interval_s, deadline)
                continue
            malformed_polls = 0
            if new_state != state:
                state = new_state
                details["state_history"].append({"state": state, "t": round(deadline.elapsed(), 3), "reason": _last_reason(last_detail)})
                log.log(f"state={state} reason={_last_reason(last_detail)!r}")

            # Token cap (BENCH_MAX_TOKENS): the same ceiling the Claude wrapper
            # enforces. Checked on every poll so a runaway run is cut short.
            if env.max_tokens is not None:
                try:
                    cost = client.cost(work_id)
                    _apply_cost(cost, result)
                    total = common.tokens_total(result)
                    if total is not None and total > env.max_tokens:
                        cancel("cancelled_after_token_cap")
                        _collect_metrics(client, work_id, last_detail, result, log)
                        return finish(OUTCOME_FAILED, f"BENCH_MAX_TOKENS exceeded: {total} > {env.max_tokens} tokens while in state {state}")
                except AEAPIError as exc:
                    log.log(f"cost poll for token cap failed (will retry next poll): {exc}")

            if state != "awaiting_user":
                awaiting_polls = 0
            elif cfg.auto_approve:
                item = last_detail.get("item") if isinstance(last_detail.get("item"), dict) else {}
                product_for_prompts = cfg.product_id or str(item.get("product_id") or "") or None
                verdict = _auto_answer_prompts(client, product_for_prompts, work_id, answered_prompts, details, deadline.elapsed(), log)
                if verdict == "answered":
                    _sleep_bounded(cfg.poll_interval_s, deadline)
                    continue
                if verdict == "none" and awaiting_polls < AWAITING_USER_GRACE_POLLS:
                    awaiting_polls += 1
                    _sleep_bounded(cfg.poll_interval_s, deadline)
                    continue
                # "blocking", or no prompt ever appeared: fall through and report blocked honestly

            if state in TERMINAL_STATES:
                break
            elif state in GATE_ENDPOINTS:
                transitions = last_detail.get("transitions") or []
                key = (state, len(transitions) if isinstance(transitions, list) else 0)
                if cfg.auto_approve and key not in approved:
                    approved.add(key)
                    endpoint = GATE_ENDPOINTS[state]
                    try:
                        client.approve(work_id, endpoint)
                        details["approvals"].append({"state": state, "endpoint": endpoint, "t": round(deadline.elapsed(), 3)})
                        log.log(f"auto-approved gate {state} via {endpoint}")
                    except AEAPIError as exc:
                        # 409 = already moved on; anything else is logged and retried on the next poll.
                        log.log(f"approve {state} via {endpoint} failed: {exc}")
                        if exc.status != 409:
                            approved.discard(key)
                elif not cfg.auto_approve:
                    log.log(f"waiting at human gate {state} (AE_AUTO_APPROVE=0)")
            _sleep_bounded(cfg.poll_interval_s, deadline)

        (env.raw_dir / "work-item-detail.json").write_text(json.dumps(last_detail, indent=2, default=str), encoding="utf-8")
        details["final_state"] = state

        # metrics (best-effort, after the run so they cover everything)
        _collect_metrics(client, work_id, last_detail, result, log)

        if timed_out:
            cancel("cancelled_after_timeout")
            return finish(OUTCOME_TIMEOUT, f"work item {work_id} still in state {state!r} after {env.time_limit_s}s")

        if state in BLOCKED_STATES:
            return finish(OUTCOME_BLOCKED, f"work item {work_id} entered {state}: {_last_reason(last_detail) or 'no reason recorded'}")
        if state in FAILED_STATES:
            return finish(OUTCOME_FAILED, f"work item {work_id} entered {state}: {_last_reason(last_detail) or 'no reason recorded'}")

        # Token cap on the final totals, fail closed: a cap that cannot be
        # verified (cost endpoint unreachable, or zero tagged inference logs)
        # is a failure, exactly as the Claude wrapper treats a missing usage block.
        if env.max_tokens is not None:
            total = common.tokens_total(result)
            if total is None:
                return finish(OUTCOME_FAILED, f"BENCH_MAX_TOKENS={env.max_tokens} is set but the work item's token usage is unknown at run end; cannot prove the cap was honored")
            if total > env.max_tokens:
                return finish(OUTCOME_FAILED, f"BENCH_MAX_TOKENS exceeded: {total} > {env.max_tokens} tokens")

        # 3. discover + checkout
        if cfg.repository_id:
            item = last_detail.get("item") if isinstance(last_detail, dict) else None
            actual_repo = str(item.get("repository_id") or "") if isinstance(item, dict) else ""
            details["repository_id_actual"] = actual_repo or None
            if actual_repo and actual_repo != cfg.repository_id:
                return finish(OUTCOME_FAILED, f"work item was assigned repository {actual_repo}, expected AE_REPOSITORY_ID={cfg.repository_id}")

        try:
            pending = [row for row in client.pending_changes() if isinstance(row, dict) and str(row.get("branch", "")).startswith(f"agent/{work_id}/")]
            details["pending_changes"] = [{k: row.get(k) for k in ("id", "branch", "base_branch", "status", "repository_id")} for row in pending]
        except AEAPIError as exc:
            log.log(f"pending-changes lookup failed (non-fatal): {exc}")

        try:
            remote_branches = list_remote_agent_branches(env.repo, cfg.fetch_remote, work_id, timeout=tail_timeout())
        except GitError as exc:
            return finish(OUTCOME_FAILED, f"ls-remote on {cfg.fetch_remote} failed: {exc}")
        details["remote_branches"] = remote_branches
        if not remote_branches:
            return finish(OUTCOME_FAILED, f"work item reached {state} but no agent/{work_id}/* branch exists on remote {cfg.fetch_remote!r}")

        try:
            head_sha, merged = checkout_produced_branches(env.repo, cfg.fetch_remote, sorted(remote_branches), log, timeout=tail_timeout())
        except GitError as exc:
            return finish(OUTCOME_FAILED, f"checkout of produced branches failed: {exc}")
        result.head_sha = head_sha
        details["checked_out_branch"] = BENCH_BRANCH
        details["merged_branches"] = merged
        # More than one subtask branch => the graded tree was assembled by the
        # wrapper, not by the product (the product QA'd each branch separately).
        details["wrapper_side_merge"] = len(merged) > 1
        if len(merged) > 1:
            log.log(f"WARNING: wrapper-side merge of {len(merged)} branches; the product never QA'd this combined tree")

        details["base_is_ancestor"] = bool(base_sha) and common.is_ancestor(env.repo, base_sha, head_sha, timeout=tail_timeout())
        if not details["base_is_ancestor"]:
            return finish(OUTCOME_FAILED, f"produced head {head_sha} does not contain benchmark base {base_sha}; the product built on a different base")

        return finish(OUTCOME_PASSED)
    except WrapperError:
        log.close()
        raise
    except Exception as exc:  # noqa: BLE001 - report honestly, never crash silently
        details["exception"] = f"{type(exc).__name__}: {exc}"
        return finish(OUTCOME_FAILED, f"wrapper exception: {type(exc).__name__}: {exc}")


def _apply_cost(cost: dict[str, Any], result: Result) -> None:
    """Map /api/work-items/cost totals onto the result.

    The endpoint sums ``inference_logs`` rows tagged with the work item or
    one of its subtasks; zero calls means UNKNOWN here, never "spent
    nothing". Since M4-05 every agent tags its calls (work item, role,
    config snapshot), and the endpoint prices them through ``model_prices``:
    ``cost_usd`` is a number only when every call with tokens has a price
    (``cost_unknown`` false); otherwise it stays null, never 0.
    """
    result.details["cost_endpoint"] = cost
    result.cost_usd = _cost_or_none(cost)
    calls = _int_or_none(cost.get("total_llm_calls"))
    if not calls:
        result.model_calls = None
        result.tokens_in = None
        result.tokens_out = None
        result.cost_usd = None
        return
    result.model_calls = calls
    result.tokens_in = _int_or_none(cost.get("total_input_tokens"))
    result.tokens_out = _int_or_none(cost.get("total_output_tokens"))


def _cost_or_none(cost: dict[str, Any]) -> float | None:
    """The priced USD cost, or None when the endpoint reports it unknown
    (no price rows, a placeholder price, or an older dashboard that has no
    ``cost_usd`` field at all)."""
    if cost.get("cost_unknown") is not False:
        return None
    value = cost.get("cost_usd")
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    return float(value)


def _collect_metrics(client: AEClient, work_id: str, detail: dict[str, Any], result: Result, log: RawLog) -> None:
    result.cost_usd = None
    try:
        _apply_cost(client.cost(work_id), result)
    except AEAPIError as exc:
        log.log(f"cost lookup failed (metrics unknown): {exc}")
        result.cost_usd = None
    agent_def = detail.get("agent_definition") if isinstance(detail, dict) else None
    if isinstance(agent_def, dict):
        result.versions["agent_definition"] = agent_def
    snapshot = detail.get("config_snapshot_id") if isinstance(detail, dict) else None
    if isinstance(snapshot, str) and snapshot:
        result.versions["config_snapshot_id"] = snapshot
    logs = detail.get("inference_logs") if isinstance(detail, dict) else None
    if isinstance(logs, list):
        models = sorted({str(entry.get("model")) for entry in logs if isinstance(entry, dict) and entry.get("model")})
        if models:
            result.versions["models"] = models


def _int_or_none(value: Any) -> int | None:
    if isinstance(value, bool) or value is None:
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


# --- CLI ----------------------------------------------------------------------


HELP = f"""Autonomous Engine competitor wrapper for scripts/code-implementation-benchmark.py.

Reads BENCH_PROMPT_FILE, submits it to the product through the gateway intake API,
polls the dashboard-backend until the work item is terminal, then fetches the
produced agent/<work_id>/* branch(es) into BENCH_REPO and checks them out.
Writes BENCH_OUTPUT_FILE (schema: benchmarks/wrappers/output_schema.json).

Runner contract (exported by the runner):
  BENCH_REPO, BENCH_PROMPT_FILE, BENCH_OUTPUT_FILE, BENCH_TASK_ID
Shared wrapper knobs:
  BENCH_TIME_LIMIT_S   wall-clock cap in seconds (default {int(common.DEFAULT_TIME_LIMIT_S)})
  BENCH_MAX_TOKENS     prompt+completion token cap, polled from /api/work-items/cost;
                       a breach cancels the work item and scores failed; unknown usage at
                       run end also scores failed (fail closed; same cap as run-claude)

Token accounting: /api/work-items/cost only sums inference logs tagged with the work
item, so tokens_in/tokens_out/model_calls are a LOWER BOUND (details.token_accounting =
"partial"); zero tagged calls is reported as unknown (null), not 0. cost_usd is
the endpoint's priced cost once Admin -> Configuration versions has prices for every
model used (model_prices), and null otherwise.
  BENCH_RAW_DIR        raw-log root, outside BENCH_REPO (default <tmp>/bench-wrappers)
Autonomous Engine:
  AE_GATEWAY_URL       intake API (default {DEFAULT_GATEWAY_URL})
  AE_DASHBOARD_URL     dashboard-backend API (default {DEFAULT_DASHBOARD_URL})
  AE_API_TOKEN         dashboard JWT (required; POST /api/auth/login)
  AE_PRODUCT_ID        product UUID (optional when exactly one product exists)
  AE_REPOSITORY_ID     repository UUID (advisory: sent as metadata, verified on the work item)
  AE_FETCH_REMOTE      REQUIRED: URL/path of the fork the product pushes agent/<work_id>/* to
                       (a fork of the fixture repo at the same base); origin is never used
  AE_POLL_INTERVAL_S   poll interval (default {int(DEFAULT_POLL_INTERVAL_S)})
  AE_AUTO_APPROVE      approve human gates automatically (default 1): the legacy
                       prd_approval/human_review states, and approval:* prompts that
                       park the item in awaiting_user. Other prompts stay blocked.
  AE_TAGS, AE_COMPLEXITY, AE_SOURCE  optional intake routing fields
  AE_CONFIG_SNAPSHOT_ID  M4-06 shadow evaluation: run this configuration snapshot (sent as
                       metadata.config_snapshot_id; the gateway honours it only with AE_ROLLOUT_ID
                       naming a rollout in shadow whose candidate/baseline it is, on a benchmark
                       scratch AE_PRODUCT_ID, and routes to the snapshot's agent definition)
  AE_ROLLOUT_ID        M4-06: the rollout the run belongs to (metadata.rollout_id)

Exit codes: 0 passed_agent, 1 failed, 2 blocked, 3 timeout, 4 usage error.
"""


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="run-autonomous-engine", description=HELP, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--show-config", action="store_true", help="print the resolved configuration (token redacted) and exit")
    args = parser.parse_args(argv)
    try:
        if args.show_config:
            cfg = AEConfig.from_environ()
            print(json.dumps(cfg.public(), indent=2, sort_keys=True))
            return 0
        return run()
    except WrapperError as exc:
        print(f"run-autonomous-engine: {exc}", file=sys.stderr)
        return EXIT_USAGE
