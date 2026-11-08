#!/usr/bin/env python3
"""Reset, seed, snapshot and drive the agent of a running instance.

Usage:
    python3 scripts/agent-harness.py snapshot [--out harness-snapshot.json]
    python3 scripts/agent-harness.py restore <snapshot.json>
    python3 scripts/agent-harness.py reset --yes
    python3 scripts/agent-harness.py seed
    python3 scripts/agent-harness.py flow <chat|mention|page|release|approve-refuse|cancel|edit>

Everything goes over HTTP to a running instance, and to its SQLite database for what the API
does not answer: which run is active, what the transcript holds. The defaults are the local
demo instance.

A flow writes what it observed to harness-report.json and exits:
    0   the flow did what it was asked to
    1   the instance did not; the report says which check failed
    2   the model provider failed ("LLM API error:"), which no change here fixes

The tools page is never posted to: a form that misses one checkbox disables every tool it does
not name, so permissions are only ever written through the database, in one transaction.
"""
import argparse
import base64
import http.cookiejar
import http.server
import json
import os
import re
import shutil
import sqlite3
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_REPOS = "alpha-diagnostics,hello-page"
DEFAULT_DB = "data/gitea.db"
DEFAULT_APP_DATA = "data/gitea"

# The agent tables a reset may touch, children first.
AGENT_TABLES = [
    "repo_pages_deployment",
    "repo_pages",
    "agent_message",
    "agent_attachment",
    "agent_tool_approval",
    "agent_run",
    "agent_conversation_folder",
    "agent_conversation",
]

ACTIVE_RUN_STATUSES = ("queued", "running", "waiting")

SEED_FILES = {
    "README.md": "# Diagnostics\n\nFirmware diagnostics for the drift rig.\n",
    "CHANGELOG.md": "# Changelog\n\n## Unreleased\n\n- Keep the drift window in one constant.\n",
    "docs/drift.md": (
        "# Drift\n\n"
        "The calibration window is `DriftWindowSeconds` in `firmware/calibration.go`, and the\n"
        "operators read it on the published page under `public/`. Both say 30 minutes (1800\n"
        "seconds) today.\n"
    ),
    "firmware/calibration.go": (
        "package firmware\n\n"
        "// DriftWindowSeconds is how long a calibration holds before the rig is re-checked.\n"
        "const DriftWindowSeconds = 1800\n"
    ),
    "public/index.html": (
        "<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n"
        "<title>Drift rig</title>\n<link rel=\"stylesheet\" href=\"style.css\">\n</head>\n"
        "<body>\n<h1>Drift rig</h1>\n<p>Calibration window: <span id=\"drift-window\">30 minutes</span></p>\n"
        "</body>\n</html>\n"
    ),
    "public/style.css": "body { font-family: sans-serif; margin: 2rem; }\n",
}

SEED_ISSUE = {
    "title": "The drift window is 10 minutes from now on",
    "body": (
        "The rig is re-checked every 10 minutes now: 600 seconds.\n\n"
        "`firmware/calibration.go` still says 1800, and the published page in `public/index.html`\n"
        "still tells the operators 30 minutes. Both have to say 10 minutes.\n"
    ),
}

FLOW_PROMPTS = {
    "chat": "Which files does this repository have at the top level, and what is the first entry in CHANGELOG.md? Answer in one short paragraph.",
    "mention": "what does docs/drift.md say the window is, and in which file is the constant? Answer in this issue.",
    "page": (
        "The drift window is 10 minutes from now on: 600 seconds. Change the constant in "
        "firmware/calibration.go and the sentence on the published page in public/index.html so "
        "both say 10 minutes, commit them on a branch of your own, open a pull request and land it "
        "on the default branch."
    ),
    "release": "Cut release v0.1.0 of this repository, using the first entry of CHANGELOG.md as the notes, and publish it.",
    "approve-refuse": (
        "On a branch called harness-refused, commit docs/refused.md with the single line \"this must not land\". "
        "Then, in a second call, commit docs/kept.md with the single line \"this lands\" on a branch called harness-kept. "
        "One commit per call, no pull request."
    ),
    "cancel": "Read every file in this repository one by one and summarise each of them at length.",
    "edit": "What does the drift window constant in firmware/calibration.go say? Answer in one line.",
    "edit-again": "How many minutes is the drift window, and which file holds the constant? Answer in one line.",
}


class HarnessError(Exception):
    pass


class Db:
    def __init__(self, path):
        if not os.path.exists(path):
            raise HarnessError(f"no database at {path}")
        self.conn = sqlite3.connect(path, timeout=20, isolation_level=None)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA busy_timeout = 20000")

    def rows(self, sql, params=()):
        return [dict(row) for row in self.conn.execute(sql, params)]

    def one(self, sql, params=()):
        rows = self.rows(sql, params)
        return rows[0] if rows else None

    def scalar(self, sql, params=()):
        row = self.one(sql, params)
        return list(row.values())[0] if row else None

    def execute(self, sql, params=()):
        return self.conn.execute(sql, params).rowcount


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """A redirect answered as itself, so a form's 303 can be told from a re-rendered page."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Instance:
    """A running instance: a signed-in session for its pages, basic auth for its API."""

    def __init__(self, url, user, password):
        self.url = url.rstrip("/")
        self.user = user
        self.password = password
        self.basic = base64.b64encode(f"{user}:{password}".encode()).decode()
        cookies = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookies))
        self.plain = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookies), NoRedirect())

    def request(self, method, path, form=None, body=None, xhr=False, json_body=None, headers=None, follow=True):
        headers = {"User-Agent": "agent-harness", **(headers or {})}
        payload = body
        if json_body is not None:
            payload = json.dumps(json_body).encode()
            headers["Content-Type"] = "application/json"
        elif form is not None:
            payload = urllib.parse.urlencode(form).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        if xhr:
            headers["X-Requested-With"] = "XMLHttpRequest"
        request = urllib.request.Request(self.url + path, data=payload, headers=headers, method=method)
        opener = self.opener if follow else self.plain
        try:
            with opener.open(request, timeout=120) as response:
                return response.status, response.read(), dict(response.headers)
        except urllib.error.HTTPError as error:
            return error.code, error.read(), dict(error.headers or {})

    def api(self, method, path, payload=None):
        headers = {"Authorization": "Basic " + self.basic, "User-Agent": "agent-harness"}
        body = None
        if payload is not None:
            body = json.dumps(payload).encode()
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(self.url + "/api/v1" + path, data=body, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=120) as response:
                raw = response.read()
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as error:
            detail = error.read().decode("utf-8", "replace")
            raise HarnessError(f"{method} /api/v1{path} -> {error.code}: {detail[:300]}") from None

    def api_optional(self, method, path, payload=None):
        """For a call whose 404 is an answer, not a failure."""
        try:
            return self.api(method, path, payload)
        except HarnessError as error:
            if "-> 404" in str(error) or "-> 409" in str(error):
                return None
            raise

    def login(self):
        status, _, _ = self.request(
            "POST", "/user/login", form={"user_name": self.user, "password": self.password}
        )
        if status != 200:
            raise HarnessError(f"could not sign in as {self.user}: HTTP {status}")
        who = self.api("GET", "/user")
        return who["login"]

    # ---- the chat surfaces -------------------------------------------------

    def open_session(self, repo_id, content):
        status, _, headers = self.request(
            "POST",
            "/-/agent/new",
            form={"kind": "repository", "repo_id": repo_id, "content": content},
            xhr=True,
        )
        if status != 204:
            raise HarnessError(f"could not open a session for repository {repo_id}: HTTP {status}")
        location = headers.get("Location", "")
        session_id = location.rsplit("/", 1)[-1]
        if not session_id.isdigit():
            raise HarnessError(f"the new session answered no usable location: {location!r}")
        return int(session_id)

    def send_message(self, session_id, content):
        status, _, _ = self.request("POST", f"/-/agent/{session_id}/messages", form={"content": content})
        if status not in (200, 204):
            raise HarnessError(f"could not send a message to session {session_id}: HTTP {status}")

    def edit_message(self, session_id, message_id, content):
        status, _, _ = self.request("POST", f"/-/agent/{session_id}/messages/{message_id}", form={"content": content})
        if status not in (200, 204):
            raise HarnessError(f"could not edit message {message_id}: HTTP {status}")

    def cancel_run(self, session_id):
        return self.request("POST", f"/-/agent/{session_id}/cancel", form={})[0]

    def reply(self, session_id, approval_id, allow):
        decision = "allow" if allow else "refuse"
        return self.request("POST", f"/-/agent/{session_id}/approvals/{approval_id}/{decision}", form={})[0]

    def approval_page(self, approval_id):
        status, body, _ = self.request("GET", f"/-/agent/approvals/{approval_id}")
        if status != 200:
            return None, None
        page = body.decode("utf-8", "replace")
        tool = re.search(r'agent-chat-approval-name">([^<]+)<', page)
        action = re.search(r'action="([^"]*/allow)"', page)
        return (tool.group(1).strip() if tool else None), (action.group(1) if action else None)


def parse_tools_page(page):
    """The permissions the tools page shows, as the form would post them back."""
    shown = {}
    for name, options in re.findall(r'<select[^>]*name="mode_([^"]+)"[^>]*>(.*?)</select>', page, re.S):
        selected = re.search(r'<option value="([^"]+)"\s*selected', options)
        shown[name] = {"mode": selected.group(1) if selected else None, "enabled": False}
    for match in re.finditer(r'<input[^>]*name="enabled_([^"]+)"[^>]*>', page):
        if match.group(1) in shown:
            shown[match.group(1)]["enabled"] = "checked" in match.group(0)
    return shown


def wait_for(predicate, timeout, interval=0.5, what="the instance"):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return last
        time.sleep(interval)
    raise HarnessError(f"timed out after {timeout:g}s waiting for {what}")


def run_of(db, run_id):
    return db.one("SELECT * FROM agent_run WHERE id = ?", (run_id,))


def newest_run(db, session_id):
    return db.one("SELECT * FROM agent_run WHERE conversation_id = ? ORDER BY id DESC LIMIT 1", (session_id,))


def pending_approvals(db, session_id):
    return db.rows(
        "SELECT * FROM agent_tool_approval WHERE conversation_id = ? AND status = 'pending' ORDER BY id",
        (session_id,),
    )


def approvals_of(db, session_id):
    return db.rows("SELECT * FROM agent_tool_approval WHERE conversation_id = ? ORDER BY id", (session_id,))


def follow(inst, db, session_id, decision=None, timeout=300, trace=None):
    """Wait for the run of a session, answering the calls that wait for a reader.

    decision(tool, index, approval) returns True to allow a call, False to refuse it.
    """
    deadline = time.monotonic() + timeout
    answered = set()
    run = None
    while time.monotonic() < deadline:
        run = newest_run(db, session_id)
        if run is None:
            time.sleep(0.5)
            continue
        if run["status"] == "waiting":
            for approval in pending_approvals(db, session_id):
                if approval["id"] in answered:
                    continue
                tool, action = inst.approval_page(approval["id"])
                if action is None:
                    continue
                allow = decision(tool or approval["tool"], len(answered), approval) if decision else True
                status = inst.reply(session_id, approval["id"], allow)
                answered.add(approval["id"])
                if trace is not None:
                    trace.append({"approval": approval["id"], "tool": tool or approval["tool"], "allowed": allow, "http": status})
        elif run["status"] not in ACTIVE_RUN_STATUSES:
            return run
        time.sleep(0.5)
    raise HarnessError(f"run of session {session_id} was still {run['status'] if run else 'absent'} after {timeout:g}s")


def transcript(db, session_id):
    return db.rows("SELECT id, role, content FROM agent_message WHERE conversation_id = ? ORDER BY id", (session_id,))


def tool_record(db, session_id, name):
    for message in transcript(db, session_id):
        if message["role"] == "tool" and message["content"].startswith(name):
            return message["content"]
    return ""


def tools_used(db, session_id, after_id=0):
    names = []
    for message in db.rows(
        "SELECT content FROM agent_message WHERE conversation_id = ? AND role = 'tool' AND id > ? ORDER BY id",
        (session_id, after_id),
    ):
        names.append(message["content"].split("\n", 1)[0].split(" ", 1)[0])
    return names


def provider_failure(run):
    return bool(run) and run.get("status") == "failed" and (run.get("error") or "").startswith("LLM API error:")


def check(checks, name, ok, detail=""):
    checks.append({"name": name, "ok": bool(ok), "detail": str(detail)[:400]})
    return bool(ok)


def write_report(path, report):
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(report, handle, indent=2, sort_keys=True)
        handle.write("\n")


def repository(inst, owner, name):
    return inst.api_optional("GET", f"/repos/{owner}/{name}")


def contents(inst, owner, name, path, ref):
    """The bytes of one file at one ref, or None when the file is not there."""
    try:
        found = inst.api("GET", f"/repos/{owner}/{name}/contents/{path}?ref={urllib.parse.quote(ref)}")
    except HarnessError as error:
        if "-> 404" in str(error):
            return None
        raise
    if isinstance(found, list):
        raise HarnessError(f"{path} is a directory")
    return base64.b64decode(found["content"])


def put_contents(inst, owner, name, path, content, message, branch="main"):
    existing = None
    try:
        existing = inst.api("GET", f"/repos/{owner}/{name}/contents/{path}?ref={urllib.parse.quote(branch)}")
    except HarnessError as error:
        if "-> 404" not in str(error):
            raise
    payload = {
        "content": base64.b64encode(content.encode()).decode(),
        "message": message,
        "branch": branch,
    }
    if existing is not None:
        payload["sha"] = existing["sha"]
    inst.api("PUT", f"/repos/{owner}/{name}/contents/{path}", payload)


def branches(inst, owner, name):
    return [entry["name"] for entry in inst.api("GET", f"/repos/{owner}/{name}/branches")]


def asked_paths(arguments):
    """The paths a commit call named, however the model wrote them down: a list, a JSON string
    of a list, or text that was encoded once more."""
    try:
        asked = json.loads(arguments)
    except ValueError:
        asked = {}
    files = asked.get("files") if isinstance(asked, dict) else None
    for _ in range(2):
        if isinstance(files, str):
            try:
                files = json.loads(files)
            except ValueError:
                files = None
    paths = [entry["path"] for entry in files if isinstance(entry, dict) and entry.get("path")] if isinstance(files, list) else []
    return paths or re.findall(r'"path"\s*:\s*"([^"]+)"', arguments)


def merged_pulls(inst, owner, name):
    return [pull for pull in inst.api("GET", f"/repos/{owner}/{name}/pulls?state=all&limit=50") if pull.get("merged")]


# ---- a model gateway one case scripts -------------------------------------


def sse(chunks):
    return "sse", chunks


def reply(message):
    return "json", {"id": "stub", "model": "stub", "choices": [{"index": 0, "message": message, "finish_reason": "stop"}]}


def called(name, arguments, call_id="call_1"):
    return {"index": 0, "id": call_id, "type": "function", "function": {"name": name, "arguments": arguments}}


def call_reply(calls, finish="tool_calls"):
    return reply({"role": "assistant", "tool_calls": calls, "finish_reason": finish})


def streamed(payload):
    """One message's worth of a stream: content or calls, as a gateway streams them."""
    if "tool_calls" in payload:
        return sse([{"model": "stub", "choices": [{"index": 0, "delta": {"tool_calls": [call]}, "finish_reason": None}]} for call in payload["tool_calls"]])
    return sse([{"model": "stub", "choices": [{"index": 0, "delta": {"content": payload.get("content", "")}, "finish_reason": None}]}])


def gateway_answer(body):
    return reply({"role": "assistant", "content": "the stub answered"})


def gateway_call(name, arguments, **kwargs):
    def build(body):
        asked = any(message.get("role") == "tool" for message in body.get("messages", []))
        return gateway_answer(body) if asked else call_reply([called(name, arguments, **kwargs)])

    return build


def gateway_calls(calls):
    def build(body):
        asked = any(message.get("role") == "tool" for message in body.get("messages", []))
        return gateway_answer(body) if asked else call_reply(calls)

    return build


def gateway_call_as_text(name, arguments):
    """A gateway that does not carry a call: the model writes it into its answer."""

    def build(body):
        asked = any(message.get("role") == "tool" for message in body.get("messages", []))
        if asked:
            return gateway_answer(body)
        parameters = "".join(
            f"\n<parameter={key}>\n{value}\n</parameter>" for key, value in json.loads(arguments).items()
        )
        text = f"Let me look at the repository.\n<tool_call>\n<function={name}>{parameters}\n</function>\n</tool_call>"
        return reply({"role": "assistant", "content": text, "finish_reason": "stop"})

    return build


def gateway_http_error(body):
    return "raw", 500, {"error": {"message": "upstream is down"}}


def gateway_stream_error(body):
    return sse([{"error": {"message": "the stream died"}}])


def gateway_malformed(body):
    return "raw", 200, b"this is not json"


def gateway_empty(body):
    return reply({"role": "assistant", "content": ""})


def gateway_no_choices(body):
    return "json", {"id": "stub", "model": "stub", "choices": []}


def gateway_round_limit(body):
    return call_reply([called("gitea_list_files", '{"path":""}')])


def gateway_chunked(body):
    if any(message.get("role") == "tool" for message in body.get("messages", [])):
        return gateway_answer(body)
    if body.get("stream"):
        return sse([
            {"model": "stub", "choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": {"name": "gitea_list_files", "arguments": '{"pa'}}]}, "finish_reason": None}]},
            {"model": "stub", "choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "function": {"arguments": 'th":""}'}}]}, "finish_reason": None}]},
        ])
    return call_reply([called("gitea_list_files", '{"path":""}')])


def gateway_arguments_object(body):
    if any(message.get("role") == "tool" for message in body.get("messages", [])):
        return gateway_answer(body)
    return call_reply([{"index": 0, "id": "call_1", "type": "function", "function": {"name": "gitea_list_files", "arguments": {"path": ""}}}])


def gateway_anonymous_call(body):
    if any(message.get("role") == "tool" for message in body.get("messages", [])):
        return gateway_answer(body)
    return call_reply([{"index": 0, "type": "function", "function": {"name": "gitea_list_files", "arguments": '{"path":""}'}}])


def gateway_unknown_tool(body):
    return gateway_call("gitea_no_such_tool", "{}")(body)


def gateway_two_calls(body):
    asked = any(message.get("role") == "tool" for message in body.get("messages", []))
    if asked:
        return gateway_answer(body)
    return call_reply([
        called("gitea_list_files", '{"path":""}', call_id="call_7"),
        {"index": 1, "id": "call_8", "type": "function", "function": {"name": "gitea_list_commits", "arguments": '{"limit":1}'}},
    ])


GATEWAY_MODES = {
    "answer": gateway_answer,
    "tool-call": gateway_call("gitea_list_files", '{"path":""}'),
    "tool-call-as-text": gateway_call_as_text("gitea_list_files", '{"path":""}'),
    "tool-unknown": gateway_unknown_tool,
    "tool-arguments-object": gateway_arguments_object,
    "tool-anonymous-call": gateway_anonymous_call,
    "tool-two-calls": gateway_two_calls,
    "tool-chunked": gateway_chunked,
    "round-limit": gateway_round_limit,
    "empty-answer": gateway_empty,
    "no-choices": gateway_no_choices,
    "http-500": gateway_http_error,
    "stream-error": gateway_stream_error,
    "malformed-json": gateway_malformed,
}


class StubGateway:
    """A model gateway a case scripts.

    The engine talks to whatever endpoint a session's model points at, so pointing it here is
    how a live instance can be made to fail, to call a tool, or to answer in a shape a real
    gateway only sometimes sends.
    """

    def __init__(self, mode, delay=0.0):
        if mode not in GATEWAY_MODES:
            raise HarnessError(f"unknown gateway mode {mode!r}: {sorted(GATEWAY_MODES)}")
        self.mode = mode
        self.delay = delay
        self.requests = []
        self.lock = threading.Lock()
        gateway = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_POST(self):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length)
                try:
                    body = json.loads(raw or b"{}")
                except ValueError:
                    body = {}
                with gateway.lock:
                    gateway.requests.append(body)
                if gateway.delay:
                    time.sleep(gateway.delay)  # a gateway that takes its time, for the queue
                status, payload = gateway.answer(body)
                stream = bool(body.get("stream"))
                if isinstance(payload, bytes):
                    data = payload
                elif stream and status == 200:
                    data = "".join(f"data: {json.dumps(chunk)}\n\n" for chunk in payload).encode() + b"data: [DONE]\n\n"
                else:
                    data = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "text/event-stream" if stream and status == 200 else "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def log_message(self, *args):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    @property
    def url(self):
        return f"http://127.0.0.1:{self.port}/v1"

    def asked(self):
        with self.lock:
            return list(self.requests)

    def asked_tools(self):
        """The tools the gateway was offered, per request."""
        return [[tool.get("function", {}).get("name") for tool in body.get("tools") or []] for body in self.asked()]

    def close(self):
        self.server.shutdown()
        self.server.server_close()

    def answer(self, body):
        result = GATEWAY_MODES[self.mode](body)
        if isinstance(result, tuple) and result and result[0] == "raw":
            return result[1], result[2]
        kind, payload = result
        if kind == "json":
            # a gateway streams when it is asked to, which is the shape the engine reads
            payload = streamed(payload["choices"][0]["message"])[1] if body.get("stream") and payload.get("choices") else payload
        # a gateway reports the model it served, which is how a run names the member that answered
        served = body.get("model")
        if served:
            for part in payload if isinstance(payload, list) else [payload]:
                if isinstance(part, dict) and part.get("choices"):
                    part["model"] = served
        return 200, payload


def role_ref(db, role):
    row = db.one("SELECT model_ref FROM agent_model_role WHERE role = ?", (role,))
    return row["model_ref"] if row else None


def set_role_ref(db, role, ref):
    db.execute(
        "INSERT INTO agent_model_role (role, model_ref, updated_unix) VALUES (?, ?, ?)"
        " ON CONFLICT(role) DO UPDATE SET model_ref = excluded.model_ref, updated_unix = excluded.updated_unix",
        (role, ref, int(time.time())),
    )


def ensure_api_key(inst, db, name, value="harness-stub-key"):
    """A key row of the harness's own, so the stub provider can be pointed at without secrets."""
    status, body, _ = inst.request("POST", "/-/admin/agent/keys/new", form={"name": name, "value": value}, follow=False)
    if status not in (302, 303, 200):
        raise HarnessError(f"could not store an api key: HTTP {status} {body[:200]!r}")
    row = db.one("SELECT id FROM agent_api_key WHERE name = ?", (name,))
    if not row:
        raise HarnessError(f"the api key {name} was not stored: {re.findall(r'message">([^<]{0,120})', body.decode('utf-8', 'replace'))}")
    return row["id"]


def create_provider(inst, db, name, base_url, model, key_id):
    status, body, _ = inst.request("POST", "/-/admin/agent/providers/new", follow=False, form={
        "name": name,
        "provider": "openai",
        "base_url": base_url,
        "api_key_id": str(key_id),
        "manual_models": model,
        "headers": "",
    })
    if status not in (302, 303):
        raise HarnessError(f"the provider form refused {name}: HTTP {status} "
                           f"{re.findall(r'message">([^<]{0,160})', body.decode('utf-8', 'replace'))}")
    row = db.one("SELECT id FROM agent_provider WHERE name = ? ORDER BY id DESC", (name,))
    if not row:
        raise HarnessError(f"the provider {name} was not stored")
    return row["id"], f"provider:{row['id']}/{model}"


def create_group(inst, db, name, mode, members):
    form = [("name", name), ("description", "harness"), ("mode", mode)]
    form.extend(("member", member) for member in members)
    status, body, _ = inst.request("POST", "/-/admin/agent/groups/new", form=form, follow=False)
    if status not in (302, 303):
        raise HarnessError(f"the group form refused {name}: HTTP {status} "
                           f"{re.findall(r'message">([^<]{0,160})', body.decode('utf-8', 'replace'))}")
    row = db.one("SELECT slug FROM agent_model_group WHERE name = ?", (name,))
    if not row:
        raise HarnessError(f"the group {name} was not stored")
    return row["slug"]


def drop_group(inst, db, slug):
    row = db.one("SELECT id FROM agent_model_group WHERE slug = ?", (slug,))
    if row:
        inst.request("POST", f"/-/admin/agent/groups/{row['id']}/delete", form={}, follow=False)


# ---- one live case at a time ----------------------------------------------


class Matrix:
    """Drives one case after another against a live instance, each with a gateway of its own.

    Every case is a branch the instance distinguishes: how a model answers, fails, calls a
    tool, or is routed through a group, and what the run, the transcript and the approvals
    then say. The role the instance had is put back when the run is over.
    """

    def __init__(self, inst, db, args):
        self.inst, self.db, self.args = inst, db, args
        self.key = ensure_api_key(inst, db, f"harness-matrix-{os.getpid()}")
        self.gateways = {}
        self.refs = {}
        self.groups = []
        self.providers = []
        self.original = role_ref(db, "repository")
        self.repo_name = args.repo or args.repos[0]
        self.repo = repository(inst, args.user, self.repo_name)
        if self.repo is None:
            raise HarnessError(f"{args.user}/{self.repo_name} is not there: seed first")

    def gateway(self, mode):
        """A gateway of this case alone: what it was asked is what this case asked."""
        gateway = StubGateway(mode)
        self.gateways[f"{mode}-{gateway.port}"] = gateway
        return gateway

    def provider(self, gateway, model="stub-model"):
        """A provider row of the harness's own, pointing at one gateway."""
        if gateway.port not in self.refs:
            provider_id, ref = create_provider(self.inst, self.db, f"harness-stub-{gateway.port}", gateway.url, model, self.key)
            self.providers.append(provider_id)
            self.refs[gateway.port] = ref
        return self.refs[gateway.port]

    def group(self, name, mode, members):
        slug = create_group(self.inst, self.db, name, mode, members)
        self.groups.append(slug)
        return f"group:{slug}"

    def use(self, ref):
        set_role_ref(self.db, "repository", ref)

    def chat(self, prompt, allow=True):
        session = self.inst.open_session(self.repo["id"], prompt)
        trace = []
        run = follow(self.inst, self.db, session, decision=(lambda *_: allow), trace=trace, timeout=self.args.timeout)
        return session, run, trace

    def message(self, prompt):
        """A session with the message in it, for the cases that only look at what was sent."""
        session = self.inst.open_session(self.repo["id"], prompt)
        run = follow(self.inst, self.db, session, timeout=self.args.timeout)
        return session, run

    def close(self):
        set_role_ref(self.db, "repository", self.original or "")
        for slug in self.groups:
            drop_group(self.inst, self.db, slug)
        for gateway in self.gateways.values():
            gateway.close()
        # the instance is left as it was found: only the harness's own rows are removed
        for provider_id in self.providers:
            self.inst.request("POST", f"/-/admin/agent/providers/{provider_id}/delete", form={})
        self.inst.request("POST", f"/-/admin/agent/keys/{self.key}/delete", form={})


def offered_of(gateway, index=0):
    asked = gateway.asked_tools()
    return asked[index] if index < len(asked) else []


def records_of(db, session, name):
    return [message for message in transcript(db, session) if message["role"] == "tool" and message["content"].startswith(name)]


def last_assistant(db, session):
    answers = [message["content"] for message in transcript(db, session) if message["role"] == "assistant"]
    return answers[-1] if answers else ""


def case_answer(mx, checks, report):
    gateway = mx.gateway("answer")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("say something")
    report["requests"] = len(gateway.asked())
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "answer.stored", last_assistant(mx.db, session) == "the stub answered", last_assistant(mx.db, session))
    check(checks, "model.named", run["model"] == "stub-model", run["model"])
    check(checks, "tools.offered", len(offered_of(gateway)) > 1, offered_of(gateway))
    check(checks, "one.request", len(gateway.asked()) == 1, len(gateway.asked()))


def case_tool_call(mx, checks, report):
    gateway = mx.gateway("tool-call")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("list the files")
    asked = gateway.asked()
    report["requests"] = len(asked)
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "tool.ran", bool(records_of(mx.db, session, "gitea_list_files")), records_of(mx.db, session, "gitea_list_files"))
    check(checks, "two.requests", len(asked) == 2, len(asked))
    if len(asked) == 2:
        replayed = [message for message in asked[1]["messages"] if message.get("role") == "tool"]
        check(checks, "tool.result.replayed", any("README.md" in (message.get("content") or "") for message in replayed), replayed)
        assistant = [message for message in asked[1]["messages"] if message.get("role") == "assistant"]
        check(checks, "call.replayed", any(message.get("tool_calls") for message in assistant), assistant)


def case_tool_call_as_text(mx, checks, report):
    """A call the model writes into its answer is a call: it runs, and the answer reads as prose."""
    gateway = mx.gateway("tool-call-as-text")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("list the files")
    asked = gateway.asked()
    report["requests"] = len(asked)
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "tool.ran", bool(records_of(mx.db, session, "gitea_list_files")), records_of(mx.db, session, "gitea_list_files"))
    check(checks, "two.requests", len(asked) == 2, len(asked))
    answers = [message["content"] for message in transcript(mx.db, session) if message["role"] == "assistant"]
    report["answers"] = [answer[:160] for answer in answers]
    check(checks, "call.not.shown", not any("<tool_call>" in (answer or "") or "<function=" in (answer or "") for answer in answers), report["answers"])
    if len(asked) == 2:
        assistant = [message for message in asked[1]["messages"] if message.get("role") == "assistant"]
        replayed = assistant[0] if assistant else {}
        report["replayed"] = replayed
        # what the model wrote beside the call is kept as the words of that round, and the call
        # travels back as a call
        check(checks, "words.kept", "Let me look at the repository." in (replayed.get("content") or ""), replayed.get("content"))
        check(checks, "call.replayed", bool(replayed.get("tool_calls")), replayed)


def case_tool_unknown(mx, checks, report):
    gateway = mx.gateway("tool-unknown")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("call something")
    record = tool_record(mx.db, session, "gitea_no_such_tool")
    report["record"] = record[:200]
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "tool.told.unknown", "unknown tool" in record, record[:200])


def case_tool_switched_off(mx, checks, report):
    gateway = mx.gateway("tool-call")
    mx.use(mx.provider(gateway))
    mx.db.execute("INSERT INTO agent_tool_permission (tool, mode, enabled) VALUES ('gitea_list_files', 'llm', 0)"
                  " ON CONFLICT(tool) DO UPDATE SET mode='llm', enabled=0")
    try:
        session, run, _ = mx.chat("list the files")
    finally:
        mx.db.execute("UPDATE agent_tool_permission SET enabled = 1 WHERE tool = 'gitea_list_files'")
    record = tool_record(mx.db, session, "gitea_list_files")
    report["record"] = record[:200]
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "tool.not.offered", "gitea_list_files" not in offered_of(gateway), offered_of(gateway))
    check(checks, "tool.told.switched_off", "switched off" in record, record[:200])


def case_tool_out_of_scope(mx, checks, report):
    """A session that works inside one repository is not handed the instance-wide tools."""
    gateway = mx.gateway("answer")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("which repositories are there")
    names = offered_of(gateway)
    report["offered"] = names
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "scope.repo.tools", "gitea_read_file" in names and "gitea_list_repos" not in names, names)


def case_round_limit(mx, checks, report):
    gateway = mx.gateway("round-limit")
    mx.use(mx.provider(gateway))
    _, run, _ = mx.chat("keep calling")
    report["requests"] = len(gateway.asked())
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "stopped.at.round_limit", "kept calling tools" in (run["error"] or ""), run["error"])
    check(checks, "rounds.bounded", len(gateway.asked()) == 6, len(gateway.asked()))


def case_empty_answer(mx, checks, report):
    gateway = mx.gateway("empty-answer")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("answer with nothing")
    report["error"] = (run["error"] or "")[:160]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "empty.answer.refused", "empty answer" in (run["error"] or ""), run["error"])


def case_no_choices(mx, checks, report):
    gateway = mx.gateway("no-choices")
    mx.use(mx.provider(gateway))
    _, run, _ = mx.chat("answer with nothing")
    report["error"] = (run["error"] or "")[:200]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "failure.is.said", bool(run["error"]), run["error"])


def case_http_500(mx, checks, report):
    gateway = mx.gateway("http-500")
    mx.use(mx.provider(gateway))
    _, run, _ = mx.chat("say something")
    report["error"] = (run["error"] or "")[:200]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "provider.failure.named", (run["error"] or "").startswith("LLM API error:"), run["error"])


def case_stream_error(mx, checks, report):
    gateway = mx.gateway("stream-error")
    mx.use(mx.provider(gateway))
    _, run, _ = mx.chat("say something")
    report["error"] = (run["error"] or "")[:200]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "stream.failure.named", "the stream died" in (run["error"] or ""), run["error"])


def case_malformed_json(mx, checks, report):
    gateway = mx.gateway("malformed-json")
    mx.use(mx.provider(gateway))
    _, run, _ = mx.chat("say something")
    report["error"] = (run["error"] or "")[:200]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "failure.is.said", bool(run["error"]), run["error"])


def case_transport_error(mx, checks, report):
    """A gateway that is not listening: the failure carries no provider prefix, only prose."""
    ref = mx.provider(mx.gateway("answer"))
    mx.db.execute("UPDATE agent_provider SET base_url = 'http://127.0.0.1:9/v1' WHERE id = ?", (int(ref.split(":")[1].split("/")[0]),))
    mx.use(ref)
    _, run, _ = mx.chat("say something")
    mx.db.execute("UPDATE agent_provider SET base_url = ? WHERE id = ?",
                  (mx.gateway("answer").url, int(ref.split(":")[1].split("/")[0])))
    error = run["error"] or ""
    report["error"] = error[:160]
    report["provider_prefix"] = error.startswith("LLM API error:")
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "failure.is.said", bool(error), error)


def case_no_api_key(mx, checks, report):
    gateway = mx.gateway("answer")
    ref = mx.provider(gateway)
    mx.db.execute("UPDATE agent_provider SET api_key_id = 0 WHERE id = ?", (int(ref.split(":")[1].split("/")[0]),))
    mx.use(ref)
    _, run, _ = mx.chat("say something")
    mx.db.execute("UPDATE agent_provider SET api_key_id = ? WHERE id = ?",
                  (mx.key, int(ref.split(":")[1].split("/")[0])))
    report["error"] = (run["error"] or "")[:160]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "key.said.missing", "no api key" in (run["error"] or ""), run["error"])


def case_no_role(mx, checks, report):
    mx.use("")
    _, run, _ = mx.chat("say something")
    report["error"] = (run["error"] or "")[:160]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "role.said.missing", "no model" in (run["error"] or "").lower(), run["error"])


def case_bad_ref(mx, checks, report):
    mx.use("not-a-ref")
    _, run, _ = mx.chat("say something")
    report["error"] = (run["error"] or "")[:160]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "failure.is.said", bool(run["error"]), run["error"])


def case_group_fallback(mx, checks, report):
    failing, working = mx.gateway("http-500"), mx.gateway("answer")
    mx.use(mx.group(f"harness-fallback-{os.getpid()}", "fallback",
                    [f"model:{mx.provider(failing, 'stub-failing').split(':')[1]}",
                     f"model:{mx.provider(working, 'stub-working').split(':')[1]}"]))
    _, run, _ = mx.chat("say something")
    report["model"] = run["model"]
    report["attempts"] = [len(failing.asked()), len(working.asked())]
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "fallback.answered", run["model"] == "stub-working", run["model"])
    check(checks, "fallback.tried.both", report["attempts"] == [1, 1], report["attempts"])


def case_group_all_fail(mx, checks, report):
    first, second = mx.gateway("http-500"), mx.gateway("stream-error")
    mx.use(mx.group(f"harness-fail-{os.getpid()}", "fallback",
                    [f"model:{mx.provider(first, 'stub-failing').split(':')[1]}",
                     f"model:{mx.provider(second, 'stub-dying').split(':')[1]}"]))
    _, run, _ = mx.chat("say something")
    report["error"] = (run["error"] or "")[:300]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "every.member.reported",
          "stub-failing" in (run["error"] or "") and "stub-dying" in (run["error"] or ""), report["error"])


def case_group_round_robin(mx, checks, report):
    first, second = mx.gateway("answer"), mx.gateway("answer")
    slug = f"harness-rr-{os.getpid()}"
    mx.use(mx.group(slug, "round_robin",
                    [f"model:{mx.provider(first, 'stub-first').split(':')[1]}",
                     f"model:{mx.provider(second, 'stub-second').split(':')[1]}"]))
    _, one, _ = mx.chat("say something")
    _, two, _ = mx.chat("say something")
    cursor = mx.db.one("SELECT cursor FROM agent_model_group WHERE slug = ?", (slug,))["cursor"]
    # two members, two answers, so the rotation is back where it started
    report.update({"first": one["model"], "second": two["model"], "cursor": cursor})
    check(checks, "rr.answers", one["status"] == "succeeded" and two["status"] == "succeeded", f"{one['status']}/{two['status']}")
    check(checks, "rr.rotates", one["model"] != two["model"], f"{one['model']} then {two['model']}")
    check(checks, "rr.cursor.saved", cursor == 0, cursor)


def case_group_nested(mx, checks, report):
    inner = mx.group(f"harness-inner-{os.getpid()}", "fallback",
                     [f"model:{mx.provider(mx.gateway('answer'), 'stub-inner').split(':')[1]}"])
    inner_id = mx.db.one("SELECT id FROM agent_model_group WHERE slug = ?", (inner.split(":")[1],))["id"]
    mx.use(mx.group(f"harness-outer-{os.getpid()}", "fallback", [f"group:{inner_id}"]))
    _, run, _ = mx.chat("say something")
    report["model"] = run["model"]
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "nested.answered", run["model"] == "stub-inner", run["model"])


def case_group_too_deep(mx, checks, report):
    """A role that contains itself is refused rather than followed forever."""
    slug = f"harness-loop-{os.getpid()}"
    first = mx.group(slug, "fallback", [f"model:{mx.provider(mx.gateway('answer'), 'stub-loop').split(':')[1]}"])
    group_id = mx.db.one("SELECT id FROM agent_model_group WHERE slug = ?", (slug,))["id"]
    mx.db.execute("DELETE FROM agent_model_group_member WHERE group_id = ?", (group_id,))
    mx.db.execute("INSERT INTO agent_model_group_member (group_id, position, member_group_id, created_unix) VALUES (?, 0, ?, ?)",
                  (group_id, group_id, int(time.time())))
    mx.use(first)
    _, run, _ = mx.chat("say something")
    report["error"] = (run["error"] or "")[:200]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "depth.refused", "too deep" in (run["error"] or ""), run["error"])


def case_tool_arguments_object(mx, checks, report):
    gateway = mx.gateway("tool-arguments-object")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("list the files")
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "object.args.ran", bool(records_of(mx.db, session, "gitea_list_files")), records_of(mx.db, session, "gitea_list_files"))


def case_anonymous_call(mx, checks, report):
    gateway = mx.gateway("tool-anonymous-call")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("list the files")
    asked = gateway.asked()
    named = []
    if len(asked) == 2:
        named = [message.get("tool_call_id") for message in asked[1]["messages"] if message.get("role") == "tool"]
    report["call_ids"] = named
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "a.call.named.back", all(named), named)


def case_two_calls(mx, checks, report):
    gateway = mx.gateway("tool-two-calls")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("list the files and the commits")
    report["records"] = [message["content"][:60] for message in transcript(mx.db, session) if message["role"] == "tool"]
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "both.tools.ran", len(records_of(mx.db, session, "gitea_list_files")) == 1
          and len(records_of(mx.db, session, "gitea_list_commits")) == 1, report["records"])


def case_chunked_call(mx, checks, report):
    gateway = mx.gateway("tool-chunked")
    mx.use(mx.provider(gateway))
    session, run, _ = mx.chat("list the files")
    record = tool_record(mx.db, session, "gitea_list_files")
    report["record"] = record[:160]
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "pieces.assembled", '{"path":""}' in record, record[:160])


def case_attachment_text(mx, checks, report):
    """A file sent with a message reaches the model as its text."""
    gateway = mx.gateway("answer")
    mx.use(mx.provider(gateway))
    session = mx.inst.open_session(mx.repo["id"], "what does the attached file say")
    status, _, _ = mx.inst.request(
        "POST", f"/-/agent/{session}/messages",
        body=multipart({"content": "what does the attached file say"},
                       [("files", "notes.txt", b"the attachment said hello")]),
        headers={"Content-Type": "multipart/form-data; boundary=" + MULTIPART_BOUNDARY},
    )
    run = follow(mx.inst, mx.db, session, timeout=mx.args.timeout) if status in (200, 204) else None
    if run is None:
        report["http"] = status
        check(checks, "attachment.accepted", False, f"HTTP {status}")
        return
    asked = gateway.asked()
    carried = any("the attachment said hello" in json.dumps(body) for body in asked)
    report["http"] = status
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "attachment.in.prompt", carried, [len(body.get("messages") or []) for body in asked])


def case_mention_failure(mx, checks, report):
    """A failing run on an issue leaves the failure in the thread, not only in the log."""
    gateway = mx.gateway("http-500")
    mx.use(mx.provider(gateway))
    agent = mx.args.agent
    issue = mx.inst.api("GET", f"/repos/{mx.args.user}/{mx.repo_name}/issues?state=open&limit=1")
    if not issue:
        check(checks, "issue.present", False, "no open issue to mention the agent in")
        return
    number, issue_id = issue[0]["number"], issue[0]["id"]
    mx.inst.api("POST", f"/repos/{mx.args.user}/{mx.repo_name}/issues/{number}/comments",
                {"body": f"@{agent} what is the drift window?"})
    session = wait_for(lambda: mx.db.one(
        "SELECT c.* FROM agent_conversation c JOIN agent_run r ON r.conversation_id = c.id"
        " WHERE r.reply_repo_id = ? AND r.reply_issue_id = ? ORDER BY r.id DESC LIMIT 1",
        (mx.repo["id"], issue_id)), 30, what="the session the mention opened")
    run = follow(mx.inst, mx.db, session["id"], timeout=mx.args.timeout)
    report["run"] = run["status"]
    # the report settles the note after the run ends, so the thread is read until it says how
    told = []
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        comments = mx.inst.api("GET", f"/repos/{mx.args.user}/{mx.repo_name}/issues/{number}/comments")
        told = [comment["body"] for comment in comments if comment["user"]["login"] == agent and "LLM API error" in comment["body"]]
        if told:
            break
        time.sleep(0.5)
    report["comments"] = [body[:160] for body in told]
    check(checks, "run.failed", run["status"] == "failed", run["status"])
    check(checks, "thread.told", bool(told), report["comments"])


def case_mention_denied(mx, checks, report):
    """A mention in a repository the agent may not read is not answered at all."""
    name = f"harness-denied-{int(time.time()) % 100000}"
    repo = mx.inst.api("POST", "/user/repos", {"name": name, "auto_init": True, "default_branch": "main", "private": True})
    try:
        issue = mx.inst.api("POST", f"/repos/{mx.args.user}/{name}/issues", {"title": "not for the agent", "body": "nothing"})
        before = mx.db.scalar("SELECT COUNT(*) FROM agent_conversation WHERE repo_id = ?", (repo["id"],))
        mx.inst.api("POST", f"/repos/{mx.args.user}/{name}/issues/{issue['number']}/comments",
                    {"body": f"@{mx.args.agent} please commit something"})
        deadline = time.monotonic() + 12
        sessions, run = None, None
        while time.monotonic() < deadline:
            sessions = mx.db.scalar("SELECT COUNT(*) FROM agent_conversation WHERE repo_id = ?", (repo["id"],))
            run = mx.db.one(
                "SELECT r.* FROM agent_run r JOIN agent_conversation c ON c.id = r.conversation_id"
                " WHERE c.repo_id = ? ORDER BY r.id DESC LIMIT 1", (repo["id"],))
            if run:
                break
            time.sleep(1)
        comments = mx.inst.api("GET", f"/repos/{mx.args.user}/{name}/issues/{issue['number']}/comments")
        answered = [comment["body"][:80] for comment in comments if comment["user"]["login"] == mx.args.agent]
        report.update({"sessions": sessions, "run": run, "agent_comments": answered})
        check(checks, "no.session", sessions == before, f"{sessions} sessions (was {before})")
        check(checks, "no.run", run is None, run)
        check(checks, "no.reply", not answered, answered)
    finally:
        mx.inst.api("DELETE", f"/repos/{mx.args.user}/{name}")


MATRIX = [
    case_answer,
    case_tool_call,
    case_tool_unknown,
    case_tool_switched_off,
    case_tool_out_of_scope,
    case_tool_arguments_object,
    case_anonymous_call,
    case_two_calls,
    case_tool_call_as_text,
    case_chunked_call,
    case_attachment_text,
    case_round_limit,
    case_empty_answer,
    case_no_choices,
    case_http_500,
    case_stream_error,
    case_malformed_json,
    case_transport_error,
    case_no_api_key,
    case_no_role,
    case_bad_ref,
    case_group_fallback,
    case_group_all_fail,
    case_group_round_robin,
    case_group_nested,
    case_group_too_deep,
    case_mention_failure,
    case_mention_denied,
]


MULTIPART_BOUNDARY = "----harness-boundary"


def multipart(fields, files):
    lines = []
    for name, value in fields.items():
        lines.append(f"--{MULTIPART_BOUNDARY}\r\n"
                     f'Content-Disposition: form-data; name="{name}"\r\n\r\n{value}\r\n')
    for name, filename, content in files:
        lines.append((f"--{MULTIPART_BOUNDARY}\r\n"
                      f'Content-Disposition: form-data; name="{name}"; filename="{filename}"\r\n'
                      f"Content-Type: text/plain\r\n\r\n").encode() + content + b"\r\n")
    body = b""
    for line in lines:
        body += line if isinstance(line, bytes) else line.encode()
    return body + f"--{MULTIPART_BOUNDARY}--\r\n".encode()


def cmd_liveness(args):
    """Every request ends: a run nothing is working on is finished, not left resting.

    Runs are planted directly in the database in the states a lost worker leaves behind, and
    the instance is watched decide them. Nothing else in the harness waits on wall time.
    """
    inst = Instance(args.url, args.user, args.password)
    inst.login()
    db = Db(args.db)
    now = int(time.time())
    stale = now - int(args.stale_age)
    planted = []
    conversation_id = None
    try:
        conversation_id = db.execute(
            "INSERT INTO agent_conversation (kind, repo_id, title, created_unix, updated_unix, owner_id, model_ref)"
            " VALUES ('repository', ?, 'harness liveness', ?, ?, 0, '')", (0, now, now))
        conversation_id = db.scalar("SELECT id FROM agent_conversation ORDER BY id DESC LIMIT 1")
        rows = [
            ("running-forever", "running", stale, True),
            ("waiting-without-a-call", "waiting", stale, True),
            ("fresh-running", "running", now - 5, True),
        ]
        for name, status, started, started_set in rows:
            db.execute(
                "INSERT INTO agent_run (conversation_id, user_id, status, created_unix, started_unix)"
                " VALUES (?, 0, ?, ?, ?)", (conversation_id, status, started, started if started_set else 0),
            )
            planted.append((name, db.scalar("SELECT id FROM agent_run ORDER BY id DESC LIMIT 1")))

        print(f"planted {len(planted)} runs in conversation {conversation_id}; waiting up to {args.wait:g}s")
        deadline = time.monotonic() + args.wait
        settled = {}
        while time.monotonic() < deadline:
            settled = {
                name: db.one("SELECT status, error FROM agent_run WHERE id = ?", (run_id,))
                for name, run_id in planted
            }
            if all(row["status"] not in ACTIVE_RUN_STATUSES for name, row in settled.items() if name != "fresh-running"):
                break
            time.sleep(2)

        checks = []
        for name, _ in planted:
            row = settled.get(name) or {}
            print(f"  {name}: {row.get('status')} {row.get('error') or ''}")
        check(checks, "stale.run.finished", (settled.get("running-forever") or {}).get("status") == "failed",
              settled.get("running-forever"))
        check(checks, "dead.call.finished", (settled.get("waiting-without-a-call") or {}).get("status") == "failed",
              settled.get("waiting-without-a-call"))
        check(checks, "running.run.left.alone", (settled.get("fresh-running") or {}).get("status") == "running",
              settled.get("fresh-running"))
        write_report(args.liveness_report, {
            "url": args.url, "conversation": conversation_id, "planted": dict(planted),
            "observed": settled, "checks": checks,
            "result": "ok" if all(entry["ok"] for entry in checks) else "failed",
        })
        print(f"liveness: {sum(1 for entry in checks if entry['ok'])}/{len(checks)} ok -> {args.liveness_report}")
        return 0 if all(entry["ok"] for entry in checks) else 1
    finally:
        for _, run_id in planted:
            db.execute("DELETE FROM agent_run WHERE id = ?", (run_id,))
        if conversation_id:
            db.execute("DELETE FROM agent_message WHERE conversation_id = ?", (conversation_id,))
            db.execute("DELETE FROM agent_conversation WHERE id = ?", (conversation_id,))


def cmd_queue(args):
    """Prove the queue on a live instance: its width, the wait it shows, and that it settles.

    The instance's own limit is read from its configuration, so nothing here changes it. The
    runs are driven through a gateway of this test's own that answers slowly and always, so what
    waits is the queue rather than a model.
    """
    limit = args.limit or configured_sessions(args.conf)
    inst = Instance(args.url, args.user, args.password)
    inst.login()
    db = Db(args.db)
    repo_name = args.repo or args.repos[0]
    repo = repository(inst, args.user, repo_name)
    if repo is None:
        raise HarnessError(f"{args.user}/{repo_name} is not there: seed first")

    original = role_ref(db, "repository")
    key = ensure_api_key(inst, db, f"harness-queue-{os.getpid()}")
    gateway = StubGateway("answer", delay=args.answer_after)
    provider_id, ref = create_provider(inst, db, f"harness-queue-{gateway.port}", gateway.url, "stub-model", key)

    sessions, peak, notes, canceled, states = [], 0, {}, None, []
    try:
        set_role_ref(db, "repository", ref)
        sessions = [inst.open_session(repo["id"], f"queue test {index}") for index in range(args.sessions)]
        print(f"asked {len(sessions)} sessions at once; the instance answers {limit} at a time")
        peak, notes, canceled, states = runQueueProbe(args, inst, db, sessions)
    finally:
        set_role_ref(db, "repository", original or "")
        gateway.close()
        inst.request("POST", f"/-/admin/agent/providers/{provider_id}/delete", form={})
        inst.request("POST", f"/-/admin/agent/keys/{key}/delete", form={}, follow=False)

    checks = []
    check(checks, "limit.respected", peak <= limit, f"{peak} ran at once, the limit is {limit}")
    check(checks, "queue.used", len(notes) >= args.min_queued,
          f"{len(notes)} sessions were seen waiting (asked at least {args.min_queued})" if len(notes) < args.min_queued else len(notes))
    places = [int(match.group(1).split(" ")[0]) for match in
              (re.search(r"\((\d+ of \d+) in the queue", note) for note in notes.values()) if match]
    waited = [note for note in notes.values() if re.search(r"waiting \d+ (s|min)", note)]
    check(checks, "place.shown", bool(notes) and len(places) == len(notes), list(notes.values())[:3])
    check(checks, "wait.shown", bool(waited), list(notes.values())[:3])
    check(checks, "places.distinct", len(set(places)) == len(places), places)
    if args.cancel:
        final = db.one("SELECT status FROM agent_run WHERE conversation_id = ? ORDER BY id DESC", (canceled,)) if canceled else None
        check(checks, "cancel.stops", bool(final) and final["status"] == "canceled", final)
    settled = db.rows("SELECT status, COUNT(*) AS n FROM agent_run WHERE conversation_id IN (%s) GROUP BY status"
                      % ",".join("?" * len(sessions)), tuple(sessions))
    check(checks, "all.settled", bool(settled) and all(row["status"] not in ACTIVE_RUN_STATUSES for row in settled), settled)

    result = "ok" if all(entry["ok"] for entry in checks) else "failed"
    write_report(args.queue_report, {
        "url": args.url, "repo": repo_name, "limit": limit, "sessions": sessions, "peak": peak,
        "notes": notes, "states": ["/".join(state) for state in states[:12]], "checks": checks, "result": result,
    })
    print(f"peak running: {peak} · sessions seen waiting: {len(notes)}")
    for entry in checks:
        print(f"  {'ok  ' if entry['ok'] else 'FAIL'} {entry['name']}: {entry['detail']}")
    print(f"queue: {result} -> {args.queue_report}")
    return 0 if result == "ok" else 1


def runQueueProbe(args, inst, db, sessions):
    """Ask, watch, and read what each waiting session says until every run has settled."""
    peak, notes, canceled, states = 0, {}, None, []
    deadline = time.monotonic() + args.timeout
    while time.monotonic() < deadline:
        rows = {row["conversation_id"]: row["status"] for row in db.rows(
            "SELECT conversation_id, status FROM agent_run WHERE conversation_id IN (%s)" % ",".join("?" * len(sessions)),
            tuple(sessions))}
        peak = max(peak, sum(1 for status in rows.values() if status == "running"))
        states.append(tuple(sorted(rows.get(session, "absent") for session in sessions)))
        queued = [session for session in sessions if rows.get(session) == "queued"]
        for session in queued:
            page = inst.request("GET", f"/-/agent/{session}")[1].decode("utf-8", "replace")
            found = re.search(r'agent-chat-pending-label">([^<]*)', page)
            if found:
                # the note is read again on every poll: a run that has waited a moment says how
                # long, which the first look at it cannot
                notes[session] = found.group(1)
        # the queue is drained by cancelling its last run once its wait has been read
        waited = any(re.search(r"waiting \d+ (s|min)", note) for note in notes.values())
        if args.cancel and canceled is None and queued and waited:
            canceled = queued[-1]
            inst.cancel_run(canceled)
        if len(rows) == len(sessions) and all(status not in ACTIVE_RUN_STATUSES for status in rows.values()):
            break
        time.sleep(0.25)
    return peak, notes, canceled, states


def configured_sessions(conf):
    """The concurrency limit the instance is configured with, as its own file says."""
    default = 4
    try:
        text = open(conf, encoding="utf-8").read()
    except OSError:
        return default
    section = re.search(r"^\[agent\]\n((?:[^\[].*\n)*)", text, re.M)
    if not section:
        return default
    found = re.search(r"^MAX_CONCURRENT_SESSIONS\s*=\s*(\d+)", section.group(1), re.M)
    return int(found.group(1)) if found and int(found.group(1)) > 0 else default


def cmd_matrix(args):
    inst = Instance(args.url, args.user, args.password)
    inst.login()
    db = Db(args.db)
    matrix = Matrix(inst, db, args)
    report = {"url": args.url, "repo": matrix.repo_name, "started": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
              "role_before": matrix.original, "cases": []}
    failed = 0
    try:
        for case in MATRIX:
            checks, detail = [], {}
            name = case.__name__.removeprefix("case_").replace("_", ".")
            try:
                case(matrix, checks, detail)
            except (HarnessError, urllib.error.URLError) as error:
                checks.append({"name": "case.ran", "ok": False, "detail": str(error)})
            ok = bool(checks) and all(entry["ok"] for entry in checks)
            failed += 0 if ok else 1
            report["cases"].append({"case": name, "ok": ok, "checks": checks, "observed": detail})
            mark = "ok  " if ok else "FAIL"
            print(f"  {mark} {name}")
            for entry in checks:
                if not entry["ok"]:
                    print(f"       {entry['name']}: {entry['detail']}")
    finally:
        matrix.close()
    report["failed"] = failed
    report["result"] = "ok" if failed == 0 else "failed"
    write_report(args.matrix_report, report)
    print(f"matrix: {len(report['cases']) - failed}/{len(report['cases'])} cases ok -> {args.matrix_report}")
    return 0 if failed == 0 else 1


# ---- commands -------------------------------------------------------------


def cmd_snapshot(args):
    inst = Instance(args.url, args.user, args.password)
    inst.login()
    db = Db(args.db)
    repos = {}
    for name in args.repos:
        repo = repository(inst, args.user, name)
        if repo is None:
            repos[name] = None
            continue
        repos[name] = {
            "id": repo["id"],
            "default_branch": repo["default_branch"],
            "branches": branches(inst, args.user, name),
        }
    snapshot = {
        "taken": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "url": args.url,
        "tool_permissions": db.rows("SELECT tool, mode, enabled FROM agent_tool_permission ORDER BY tool"),
        "skills": db.rows("SELECT name, description, body, scope, enabled FROM agent_skill ORDER BY name"),
        "repos": repos,
    }
    write_report(args.out, snapshot)
    print(
        f"snapshot: {len(snapshot['tool_permissions'])} tool permissions, "
        f"{len(snapshot['skills'])} skills, {sum(1 for r in repos.values() if r)} repositories -> {args.out}"
    )
    return 0


def cmd_restore(args):
    inst = Instance(args.url, args.user, args.password)
    inst.login()
    db = Db(args.db)
    with open(args.file, encoding="utf-8") as handle:
        snapshot = json.load(handle)

    db.conn.execute("BEGIN IMMEDIATE")
    try:
        db.conn.execute("DELETE FROM agent_tool_permission")
        for row in snapshot["tool_permissions"]:
            db.conn.execute(
                "INSERT INTO agent_tool_permission (tool, mode, enabled) VALUES (?, ?, ?)",
                (row["tool"], row["mode"], int(row["enabled"])),
            )
        db.conn.execute("DELETE FROM agent_skill")
        for row in snapshot["skills"]:
            db.conn.execute(
                "INSERT INTO agent_skill (name, description, body, scope, enabled, created_unix, updated_unix)"
                " VALUES (?, ?, ?, ?, ?, ?, ?)",
                (
                    row["name"],
                    row["description"],
                    row["body"],
                    row.get("scope") or "common",
                    int(row["enabled"]),
                    int(time.time()),
                    int(time.time()),
                ),
            )
        db.conn.execute("COMMIT")
    except Exception:
        db.conn.execute("ROLLBACK")
        raise

    status, body, _ = inst.request("GET", "/-/admin/agent/tools")
    if status != 200:
        raise HarnessError(f"the tools page answered HTTP {status}")
    shown = parse_tools_page(body.decode("utf-8", "replace"))
    wanted = {row["tool"]: row for row in snapshot["tool_permissions"]}
    wrong = []
    for tool, row in wanted.items():
        here = shown.get(tool)
        if here is None:
            wrong.append(f"{tool}: not on the page")
        elif here["mode"] != row["mode"] or here["enabled"] != bool(row["enabled"]):
            wrong.append(f"{tool}: page says {here['mode']}/{here['enabled']}, snapshot says {row['mode']}/{bool(row['enabled'])}")
    for tool, here in shown.items():
        if tool not in wanted:
            wrong.append(f"{tool}: on the page but not in the snapshot")

    skills = db.rows("SELECT name, enabled FROM agent_skill ORDER BY name")
    restored = {row["name"]: bool(row["enabled"]) for row in skills}
    expected = {row["name"]: bool(row["enabled"]) for row in snapshot["skills"]}
    if restored != expected:
        wrong.append(f"skills: {restored} != {expected}")
    if wrong:
        print("restore failed:", file=sys.stderr)
        for line in wrong:
            print("  " + line, file=sys.stderr)
        return 1
    print(f"restore: {len(wanted)} tool permissions and {len(restored)} skills match the snapshot")
    return 0


def cmd_reset(args):
    inst = Instance(args.url, args.user, args.password)
    inst.login()
    db = Db(args.db)

    print("will delete, in this order:")
    for name in args.repos:
        repo = repository(inst, args.user, name)
        if repo is None:
            print(f"  repository {args.user}/{name}: not there")
        else:
            print(
                f"  repository {args.user}/{name}: id {repo['id']}, "
                f"{len(branches(inst, args.user, name))} branches, {repo['open_issues_count']} open issues"
            )
    counts = {table: db.scalar(f"SELECT COUNT(*) FROM {table}") for table in AGENT_TABLES}
    for table in AGENT_TABLES:
        print(f"  rows in {table}: {counts[table]}")
    for path in (os.path.join(args.app_data_path, "agent-attachments"), os.path.join(args.app_data_path, "agent-prompt-traces")):
        files = sum(len(names) for _, _, names in os.walk(path))
        print(f"  {path}: {files} files")
    if not args.yes:
        print("nothing was deleted: pass --yes")
        return 1

    for name in args.repos:
        if repository(inst, args.user, name) is None:
            print(f"repository {args.user}/{name}: already gone")
            continue
        inst.api("DELETE", f"/repos/{args.user}/{name}")
        print(f"repository {args.user}/{name}: deleted")

    db.conn.execute("BEGIN IMMEDIATE")
    try:
        for table in AGENT_TABLES:
            deleted = db.conn.execute(f"DELETE FROM {table}").rowcount
            print(f"rows deleted from {table}: {deleted}")
        db.conn.execute("COMMIT")
    except Exception:
        db.conn.execute("ROLLBACK")
        raise
    for path in (os.path.join(args.app_data_path, "agent-attachments"), os.path.join(args.app_data_path, "agent-prompt-traces")):
        shutil.rmtree(path, ignore_errors=True)
        print(f"{path}: removed")
    return 0


def role_refs_resolving(db):
    """What each role names and, when it names nothing, why it can not be answered with."""
    providers = {row["id"] for row in db.rows("SELECT id FROM agent_provider")}
    groups = {row["slug"] for row in db.rows("SELECT slug FROM agent_model_group")}
    report = []
    for row in db.rows("SELECT role, model_ref FROM agent_model_role ORDER BY role"):
        ref, why = row["model_ref"], ""
        kind, _, rest = ref.partition(":")
        if not ref:
            why = "no model assigned: sessions of this role are not answered"
        elif kind == "provider" and int(rest.split("/")[0] or 0) not in providers:
            why = "the provider it names is gone"
        elif kind == "group" and rest not in groups:
            why = "the group it names is gone"
        report.append((row["role"], ref, why))
    return report


def cmd_seed(args):
    inst = Instance(args.url, args.user, args.password)
    inst.login()
    for role, ref, why in role_refs_resolving(Db(args.db)):
        print(f"role {role}: {ref or 'none'}" + (f" -- {why}" if why else ""))
    for name in args.repos:
        if repository(inst, args.user, name) is not None:
            raise HarnessError(f"{args.user}/{name} is already there: reset first")
        inst.api("POST", "/user/repos", {"name": name, "auto_init": True, "default_branch": "main", "private": False})
        wait_for(lambda: repository(inst, args.user, name), 30, what=f"the new repository {name}")
        wait_for(lambda: branches(inst, args.user, name), 30, what=f"the first commit of {name}")
        print(f"repository {args.user}/{name}: created")
        inst.api("PUT", f"/repos/{args.user}/{name}/collaborators/{args.agent}", {"permission": "write"})
        print(f"collaborator {args.agent} on {name}: write")
        for path, content in SEED_FILES.items():
            put_contents(inst, args.user, name, path, content, f"seed {path}")
        print(f"files seeded in {name}: {len(SEED_FILES)}")
        issue = inst.api("POST", f"/repos/{args.user}/{name}/issues", SEED_ISSUE)
        print(f"issue {name}#{issue['number']}: {issue['title']}")
    return 0


def flow_page(inst, db, args, repo, checks, report):
    # the flow is about a change that is not there yet: on a repository that already carries it
    # the model has nothing to do, and its silence would read as a failure of the instance
    def at_main(path):
        found = contents(inst, args.user, repo["name"], path, "main")
        return found.decode("utf-8", "replace") if found is not None else None

    before_prs = {pull["number"] for pull in merged_pulls(inst, args.user, repo["name"])}
    seeded = (
        at_main("public/index.html") == SEED_FILES["public/index.html"]
        and at_main("firmware/calibration.go") == SEED_FILES["firmware/calibration.go"]
    )
    report["precondition"] = {"seeded": seeded, "merged_pulls": sorted(before_prs)}
    if not check(checks, "precondition.seeded", seeded, "the repository is not as seeded: run reset and seed" if not seeded else "as seeded"):
        return

    session = inst.open_session(repo["id"], FLOW_PROMPTS["page"])
    trace = []
    run = follow(inst, db, session, trace=trace, timeout=args.timeout)
    report["session"] = session
    report["run"] = run
    report["approvals"] = trace
    if provider_failure(run):
        return
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    tools = {entry["tool"] for entry in trace}
    for tool in ("gitea_commit_files", "gitea_create_pull_request", "gitea_merge_pull_request"):
        check(checks, f"approvals.{tool}", tool in tools, sorted(tools))
    check(checks, "approvals.all_allowed", all(entry["allowed"] for entry in trace), trace)

    pulls = [pull for pull in merged_pulls(inst, args.user, repo["name"]) if pull["number"] not in before_prs]
    report["pr"] = {"merged": bool(pulls), "count": len(pulls)}
    if pulls:
        report["pr"]["index"] = pulls[-1]["number"]
        report["pr"]["title"] = pulls[-1]["title"]
    if not check(checks, "pr.merged", bool(pulls), f"{len(pulls)} merged since the flow started"):
        return

    # the pages of the repository are published from the public folder of its default branch
    inst.api("PUT", f"/repos/{args.user}/{repo['name']}/pages", {"source": "branch", "branch": "main", "dir": "/public"})
    status, body, headers = inst.request("GET", f"/{args.user}/{repo['name']}/-/pages/")
    served = body.decode("utf-8", "replace")
    content_type = headers.get("Content-Type", "")
    report["site"] = {"http": status, "content_type": content_type}
    check(checks, "site.http", status == 200, status)
    check(checks, "site.content_type", content_type.startswith("text/html"), content_type)

    page = contents(inst, args.user, repo["name"], "public/index.html", "main")
    page = page.decode("utf-8", "replace") if page is not None else None
    report["page"] = {"changed": page is not None and page != SEED_FILES["public/index.html"]}
    check(checks, "page.changed", report["page"]["changed"], repr(page)[:200])
    check(checks, "site.matches_branch", page == served, f"{len(page or '')} vs {len(served)} bytes")
    check(checks, "page.says_ten_minutes", "10 minute" in (page or ""), repr(page)[:200])

    calibration = contents(inst, args.user, repo["name"], "firmware/calibration.go", "main")
    calibration = calibration.decode("utf-8", "replace") if calibration is not None else ""
    report["calibration"] = {"changed": calibration != SEED_FILES["firmware/calibration.go"], "says": "600" in calibration}
    check(checks, "calibration.changed", report["calibration"]["changed"], calibration[:200])
    check(checks, "calibration.says_600", "600" in calibration and "1800" not in calibration, calibration[:200])

    status, body, _ = inst.request("GET", f"/{args.user}/{repo['name']}")
    report["page"]["nav_link"] = "/-/pages/" in body.decode("utf-8", "replace")
    check(checks, "page.nav_link", report["page"]["nav_link"], status)


def flow_chat(inst, db, args, repo, checks, report):
    session = inst.open_session(repo["id"], FLOW_PROMPTS["chat"])
    run = follow(inst, db, session, timeout=args.timeout)
    report["session"] = session
    report["run"] = run
    if provider_failure(run):
        return
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    used = tools_used(db, session)
    report["tools"] = used
    check(checks, "tools.read", bool({"gitea_list_files", "gitea_read_file"} & set(used)), used)
    answers = [message["content"] for message in transcript(db, session) if message["role"] == "assistant"]
    report["answer"] = answers[-1][:400] if answers else ""
    check(checks, "answer.written", bool(answers and answers[-1].strip()), report["answer"][:120])


def flow_mention(inst, db, args, repo, checks, report):
    issues = inst.api("GET", f"/repos/{args.user}/{repo['name']}/issues?state=open&limit=1")
    if not issues:
        raise HarnessError(f"{args.user}/{repo['name']} has no open issue to mention the agent in")
    number, issue_id = issues[0]["number"], issues[0]["id"]
    inst.api(
        "POST",
        f"/repos/{args.user}/{repo['name']}/issues/{number}/comments",
        {"body": f"@{args.agent} " + FLOW_PROMPTS["mention"]},
    )
    report["issue"] = number
    # the mention answers in the session of that repository, which may not be the newest one:
    # the run it started is the one that names the issue it came from
    session = wait_for(
        lambda: db.one(
            "SELECT c.* FROM agent_conversation c JOIN agent_run r ON r.conversation_id = c.id"
            " WHERE r.reply_repo_id = ? AND r.reply_issue_id = ? ORDER BY r.id DESC LIMIT 1",
            (repo["id"], issue_id),
        ),
        30,
        what="the session the mention opened",
    )
    report["session"] = session["id"]
    run = follow(inst, db, session["id"], timeout=args.timeout)
    report["run"] = run
    if provider_failure(run):
        return
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    told = [
        message["content"]
        for message in transcript(db, session["id"])
        if message["role"] == "user" and "[mention]" in message["content"]
    ]
    check(checks, "run.told_about_mention", bool(told), "")
    agent_comments = [
        comment for comment in inst.api("GET", f"/repos/{args.user}/{repo['name']}/issues/{number}/comments")
        if comment["user"]["login"] == args.agent
    ]
    report["agent_comments"] = [comment["body"][:200] for comment in agent_comments]
    check(checks, "thread.answered", any(comment["body"].strip() for comment in agent_comments), len(agent_comments))


def flow_release(inst, db, args, repo, checks, report):
    session = inst.open_session(repo["id"], FLOW_PROMPTS["release"])
    trace = []
    run = follow(inst, db, session, trace=trace, timeout=args.timeout)
    report["session"] = session
    report["run"] = run
    report["approvals"] = trace
    if provider_failure(run):
        return
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    check(checks, "approval.create_release", "gitea_create_release" in {entry["tool"] for entry in trace}, trace)
    releases = inst.api("GET", f"/repos/{args.user}/{repo['name']}/releases")
    report["releases"] = [release["tag_name"] for release in releases]
    check(checks, "release.published", bool(releases), report["releases"])


def flow_approve_refuse(inst, db, args, repo, checks, report):
    def decision(tool, index, approval):
        return index == 0

    session = inst.open_session(repo["id"], FLOW_PROMPTS["approve-refuse"])
    trace = []
    run = follow(inst, db, session, decision=decision, trace=trace, timeout=args.timeout)
    report["session"] = session
    report["run"] = run
    report["approvals"] = trace
    if provider_failure(run):
        return
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])
    allowed = [entry for entry in trace if entry["allowed"]]
    refused = [entry for entry in trace if not entry["allowed"]]
    check(checks, "approval.allowed", bool(allowed), allowed)
    check(checks, "approval.refused", bool(refused), refused)

    rows = approvals_of(db, session)
    report["approval_rows"] = [{"tool": row["tool"], "status": row["status"], "arguments": row["arguments"][:200]} for row in rows]
    check(checks, "approval_rows.refused", any(row["status"] == "refused" for row in rows), rows)

    # what a refused call asked to write is nowhere, on any branch
    paths = [
        path
        for row in rows
        if row["status"] == "refused" and row["tool"] == "gitea_commit_files"
        for path in asked_paths(row["arguments"])
    ]
    everywhere = []
    for branch in branches(inst, args.user, repo["name"]):
        for path in paths:
            if contents(inst, args.user, repo["name"], path, branch) is not None:
                everywhere.append(f"{branch}:{path}")
    report["refused_paths"] = paths
    check(checks, "refused.change_absent", bool(paths) and not everywhere, everywhere or f"paths={paths}")

    # and what the reader allowed is on the branch the call named
    landed, missing = [], []
    for row in rows:
        if row["status"] != "allowed" or row["tool"] != "gitea_commit_files":
            continue
        try:
            branch = json.loads(row["arguments"]).get("branch") or repo["default_branch"]
        except ValueError:
            continue
        for path in asked_paths(row["arguments"]):
            found = contents(inst, args.user, repo["name"], path, branch)
            (landed if found is not None else missing).append(f"{branch}:{path}")
    report["approved_paths"] = landed
    check(checks, "approved.change_landed", bool(landed) and not missing, missing or landed)


def flow_cancel(inst, db, args, repo, checks, report):
    session = inst.open_session(repo["id"], FLOW_PROMPTS["cancel"])
    report["session"] = session
    run = wait_for(
        lambda: (
            lambda latest: latest if latest and latest["status"] in ACTIVE_RUN_STATUSES else None
        )(newest_run(db, session)),
        30,
        what="the run to start",
    )
    report["run_started"] = run
    status = inst.cancel_run(session)
    report["cancel_http"] = status
    check(checks, "cancel.accepted", status in (200, 204), status)
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        run = newest_run(db, session)
        if run["status"] not in ACTIVE_RUN_STATUSES:
            break
        time.sleep(0.5)
    report["run"] = run
    check(checks, "run.canceled", run["status"] == "canceled", run["status"] + " " + (run["error"] or ""))


def flow_edit(inst, db, args, repo, checks, report):
    session = inst.open_session(repo["id"], FLOW_PROMPTS["edit"])
    run = follow(inst, db, session, timeout=args.timeout)
    report["session"] = session
    report["run"] = run
    if provider_failure(run):
        return
    check(checks, "run.succeeded", run["status"] == "succeeded", run["error"])

    said = [message for message in transcript(db, session) if message["role"] == "user"][-1]
    inst.edit_message(session, said["id"], FLOW_PROMPTS["edit-again"])
    second = wait_for(
        lambda: (
            lambda latest: latest if latest and latest["id"] > run["id"] and latest["status"] not in ACTIVE_RUN_STATUSES else None
        )(newest_run(db, session)),
        args.timeout,
        what="the run the edit starts",
    )
    report["edited_run"] = second
    check(checks, "edit.started_a_run", second["id"] > run["id"], f"{run['id']} -> {second['id']}")
    check(checks, "edit.run_succeeded", second["status"] == "succeeded", second["error"])
    now = [message for message in transcript(db, session) if message["role"] == "user"][-1]
    report["edited_message"] = now["content"][:200]
    check(checks, "edit.stored", now["content"] == FLOW_PROMPTS["edit-again"], now["content"][:120])


FLOWS = {
    "chat": flow_chat,
    "mention": flow_mention,
    "page": flow_page,
    "release": flow_release,
    "approve-refuse": flow_approve_refuse,
    "cancel": flow_cancel,
    "edit": flow_edit,
}


def cmd_flow(args):
    inst = Instance(args.url, args.user, args.password)
    who = inst.login()
    db = Db(args.db)
    repo_name = args.repo or args.repos[0]
    repo = repository(inst, args.user, repo_name)
    if repo is None:
        raise HarnessError(f"{args.user}/{repo_name} is not there: seed first")
    report = {
        "flow": args.flow,
        "url": args.url,
        "user": who,
        "repo": repo_name,
        "started": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
    }
    checks = []
    FLOWS[args.flow](inst, db, args, repo, checks, report)
    # a provider that answered with an error is nobody's change here: the run stops before it
    # can be about the product at all
    run = report.get("run") or {}
    if provider_failure(run) or provider_failure(report.get("edited_run")):
        checks.append({"name": "provider.failed", "ok": False, "detail": (run.get("error") or "")[:400]})
        report["result"] = "provider"
    else:
        report["result"] = "ok" if all(entry["ok"] for entry in checks) else "failed"
    report["checks"] = checks
    write_report(args.report, report)

    print(f"flow {args.flow} on {repo_name}: {report['result']}")
    print(f"  run {run.get('id')} {run.get('status')} {(run.get('error') or '')[:200]}")
    for entry in checks:
        mark = "ok  " if entry["ok"] else "FAIL"
        print(f"  {mark} {entry['name']} {entry['detail']}")
    print(f"  report: {args.report}")
    return {"ok": 0, "failed": 1, "provider": 2}[report["result"]]


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0], formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--url", default="http://10.0.0.6:3003")
    parser.add_argument("--user", default="admin")
    parser.add_argument("--password", default="admin123")
    parser.add_argument("--db", default=DEFAULT_DB)
    parser.add_argument("--app-data-path", default=DEFAULT_APP_DATA)
    parser.add_argument("--repos", default=DEFAULT_REPOS, help="comma separated, the demo repositories")
    parser.add_argument("--agent", default="agent", help="the account the agent writes as")
    parser.add_argument("--repo", default="", help="the repository a flow drives (default: the first of --repos)")
    parser.add_argument("--report", default="harness-report.json")
    parser.add_argument("--timeout", type=float, default=300, help="how long one flow waits for its run")
    sub = parser.add_subparsers(dest="command", required=True)

    snapshot = sub.add_parser("snapshot", help="write down the permissions, skills and repositories")
    snapshot.add_argument("--out", default="harness-snapshot.json")
    snapshot.set_defaults(func=cmd_snapshot)

    restore = sub.add_parser("restore", help="write a snapshot back and check the tools page")
    restore.add_argument("file")
    restore.set_defaults(func=cmd_restore)

    reset = sub.add_parser("reset", help="delete the demo repositories, the agent's rows and its files")
    reset.add_argument("--yes", action="store_true", help="actually delete; without it only prints")
    reset.set_defaults(func=cmd_reset)

    seed = sub.add_parser("seed", help="create the demo repositories, their files and one issue")
    seed.set_defaults(func=cmd_seed)

    queue = sub.add_parser("queue", help="ask for more sessions than the instance answers at once")
    queue.add_argument("--sessions", type=int, default=0, help="how many sessions ask at once (default: the limit plus two)")
    queue.add_argument("--limit", type=int, default=0, help="the instance's limit, when its config is elsewhere")
    queue.add_argument("--conf", default="custom/conf/app.ini", help="the instance's config, for its limit")
    queue.add_argument("--min-queued", type=int, default=1, help="how many sessions have to be seen waiting")
    queue.add_argument("--answer-after", type=float, default=1.0,
                       help="how long the gateway the test drives takes to answer")
    queue.add_argument("--cancel", action="store_true", help="cancel a waiting run and check it stops")
    queue.add_argument("--queue-report", default="harness-queue.json")
    queue.add_argument("--repo", default=argparse.SUPPRESS)
    queue.add_argument("--timeout", type=float, default=600)
    queue.set_defaults(func=cmd_queue)

    liveness = sub.add_parser("liveness", help="plant stranded runs and watch the instance finish them")
    liveness.add_argument("--liveness-report", default="harness-liveness.json")
    liveness.add_argument("--stale-age", type=float, default=13 * 60,
                          help="how old a planted run is, past the engine's own deadline")
    liveness.add_argument("--wait", type=float, default=100, help="how long to watch before giving up")
    liveness.set_defaults(func=cmd_liveness)

    matrix = sub.add_parser("matrix", help="run every case of the model interaction against the gateway it scripts")
    matrix.add_argument("--matrix-report", default="harness-matrix.json")
    matrix.add_argument("--repo", default=argparse.SUPPRESS)
    matrix.add_argument("--timeout", type=float, default=argparse.SUPPRESS)
    matrix.set_defaults(func=cmd_matrix)

    flow = sub.add_parser("flow", help="drive one flow end to end and write a report")
    flow.add_argument("flow", choices=sorted(FLOWS))
    # the same three, so a flow can be pointed at one repository without repeating the globals
    flow.add_argument("--repo", default=argparse.SUPPRESS)
    flow.add_argument("--report", default=argparse.SUPPRESS)
    flow.add_argument("--timeout", type=float, default=argparse.SUPPRESS)
    flow.set_defaults(func=cmd_flow)

    args = parser.parse_args(argv)
    args.repos = [name.strip() for name in args.repos.split(",") if name.strip()]
    if hasattr(args, "sessions") and args.sessions == 0:
        args.sessions = (getattr(args, "limit", 0) or configured_sessions(args.conf)) + 2
    try:
        return args.func(args)
    except (HarnessError, urllib.error.URLError) as error:
        print(f"failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
