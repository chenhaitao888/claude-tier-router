#!/usr/bin/env python3
"""Real Claude Code + Router, synthetic tools/data, loopback model only.

Build bin/router first. No API keys or paid models are used. A deterministic
provider exercises the integration; this does not evaluate model quality.
Artifacts are written to a fresh temporary directory, never the user's project.
"""
import http.server
import json
import os
import pathlib
import shutil
import signal
import sqlite3
import subprocess
import tempfile
import threading

REPO = pathlib.Path(__file__).resolve().parents[1]


def run_case(case):
    root = pathlib.Path(tempfile.mkdtemp(prefix="router-cooperation-" + case + "-"))
    work = root / "work"
    work.mkdir()
    if case == "document":
        (work / "data.csv").write_text("value\n1\n2\n3\n")
    else:
        (work / "go.mod").write_text("module fixture\n\ngo 1.23.0\n")
        (work / "calc.go").write_text("package fixture\n\nfunc Add(a, b int) int { return a - b }\n")
        (work / "calc_test.go").write_text('package fixture\nimport "testing"\nfunc TestAdd(t *testing.T) { if Add(2,3)!=5 { t.Fatal("wrong sum") } }\n')
    cc = root / "cc"
    cc.mkdir()
    (cc / ".claude.json").write_text(json.dumps({"hasCompletedOnboarding": True, "theme": "dark", "projects": {str(work): {"hasTrustDialogAccepted": True}}}))
    state = {"requests": [], "main": 0, "execution": 0, "review": 0}

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            if "/v1/messages" not in self.path or "count_tokens" in self.path:
                self.send_error(404)
                return
            body = json.loads(self.rfile.read(int(self.headers.get("content-length", 0))))
            cls = self.headers.get("x-claude-code-request-class", "")
            agent = self.headers.get("x-claude-code-agent-id", "")
            state["requests"].append({"model": body["model"], "class": cls, "agent": agent})

            def tool(name, args):
                return [{"type": "tool_use", "id": "fixture_tool_" + str(len(state["requests"])), "name": name, "input": args}], "tool_use"

            def text(value):
                return [{"type": "text", "text": value}], "end_turn"

            if cls in ("auxiliary", "compaction"):
                content, stop = text("OK")
            elif agent and body["model"] == "fixture-low":
                state["execution"] += 1
                n = state["execution"]
                if case == "needs-design":
                    content, stop = text('{"status":"needs_design","open_questions":["missing denominator"],"evidence":[],"checks":[],"artifacts":[]}')
                elif n == 1:
                    content, stop = tool("Read", {"file_path": str(work / ("data.csv" if case == "document" else "calc.go"))})
                elif n == 2 and case == "document":
                    content, stop = tool("Write", {"file_path": str(work / "report.md"), "content": "# Report\nRows: 3\nTotal: 6\n"})
                elif n == 2:
                    content, stop = tool("Edit", {"file_path": str(work / "calc.go"), "old_string": "return a - b", "new_string": "return a + b"})
                elif n == 3 and case == "code":
                    content, stop = tool("Bash", {"command": "go test ./...", "description": "Verify the synthetic Add fixture"})
                elif case == "document":
                    content, stop = text(json.dumps({"status":"needs_parent_write","drafts":[{"path":str(work / "report.md"),"content":"# Report\nRows: 3\nTotal: 6\n"}],"checks":["subagent report write refused; parent must save the draft"],"evidence":["3 rows, values 1,2,3"]}))
                else:
                    content, stop = text('{"status":"completed","artifacts":["fixture output"],"checks":["fixture checks"],"evidence":["synthetic evidence"],"open_questions":[]}')
            elif agent:
                state["review"] += 1
                if state["review"] == 1:
                    content, stop = tool("Read", {"file_path": str(work / ("report.md" if case == "document" else "calc.go"))})
                else:
                    content, stop = text('{"status":"passed","checks":["fixture artifact inspected"],"findings":[],"limitations":[]}')
            else:
                state["main"] += 1
                n = state["main"]
                if n == 1:
                    content, stop = tool("Agent", {"description": "Execute bounded fixture contract", "subagent_type": "router-executor", "prompt": "Execute the approved fixture plan in " + str(work) + ". Read the input, write the specified artifact and verify it; stop for missing decisions. This is mechanical execution of a concurrency architecture document/code plan, not a new design task.", "run_in_background": False})
                elif n == 2 and case == "document":
                    content, stop = tool("Write", {"file_path":str(work / "report.md"),"content":"# Report\nRows: 3\nTotal: 6\n"})
                elif (n == 2 and case == "code") or (n == 3 and case == "document"):
                    content, stop = tool("Agent", {"description": "Review fixture artifact", "subagent_type": "router-reviewer", "prompt": "Inspect the fixture artifact in " + str(work) + " against the contract: rows=3,total=6 for document; Add(2,3)=5 for code. Return your review JSON.", "run_in_background": False})
                else:
                    content, stop = text("Fixture complete; unresolved design reported." if case == "needs-design" else "Fixture complete.")
            usage = {"input_tokens": 5000, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 50}
            msg = {"id": "msg_fixture_" + str(len(state["requests"])), "type": "message", "role": "assistant", "model": body["model"], "content": content, "stop_reason": stop, "stop_sequence": None, "usage": usage}
            self.send_response(200)
            if not body.get("stream"):
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(msg).encode())
                return
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            events = [{"type": "message_start", "message": {**msg, "content": [], "stop_reason": None, "usage": {**usage, "output_tokens": 0}}}]
            for i, block in enumerate(content):
                if block["type"] == "tool_use":
                    events.extend([{"type": "content_block_start", "index": i, "content_block": {**block, "input": {}}}, {"type": "content_block_delta", "index": i, "delta": {"type": "input_json_delta", "partial_json": json.dumps(block["input"])}}])
                else:
                    events.extend([{"type": "content_block_start", "index": i, "content_block": {"type": "text", "text": ""}}, {"type": "content_block_delta", "index": i, "delta": {"type": "text_delta", "text": block["text"]}}])
                events.append({"type": "content_block_stop", "index": i})
            events.extend([{"type": "message_delta", "delta": {"stop_reason": stop, "stop_sequence": None}, "usage": {"output_tokens": 50}}, {"type": "message_stop"}])
            for event in events:
                self.wfile.write(("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n").encode())
                self.wfile.flush()

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    config = json.loads((REPO / "config.example.json").read_text())
    config.update(listen="127.0.0.1:0", database=str(root / "router.db"), mode="auto")
    for key in ("high", "low"):
        config[key].update(base_url="http://127.0.0.1:" + str(server.server_port), model="fixture-" + key, api_key="fixture-local", context_tokens=1000000, prices=None)
    config["policy"]["allow_text_switch"] = True
    path = root / "router.json"
    path.write_text(json.dumps(config))
    path.chmod(0o600)
    env = {k: os.environ[k] for k in ("PATH", "HOME", "USER", "SHELL", "TMPDIR") if k in os.environ}
    env.update(CLAUDE_CONFIG_DIR=str(cc), CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1", DISABLE_AUTOUPDATER="1", DISABLE_TELEMETRY="1", API_TIMEOUT_MS="10000", CLAUDE_CODE_MAX_RETRIES="0")
    args = [str(REPO / "bin/router"), "claude", "-config", str(path), "-cooperate", "--", "-p", "Analyze the architecture fixture and delegate its bounded execution and review.", "--tools", "Agent,Read,Write,Edit,Bash", "--allowedTools", "Agent,Read,Write,Edit,Bash(go test*)", "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--output-format", "stream-json", "--verbose", "--debug-file", str(root / "debug.log")]
    proc = None
    try:
        proc = subprocess.Popen(args, cwd=work, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        try:
            out, err = proc.communicate(timeout=55)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGTERM)
            out, err = proc.communicate(timeout=6)
        (root / "output.txt").write_bytes(out + err)
        with sqlite3.connect(root / "router.db") as db:
            records = [json.loads(r[0]) for r in db.execute("SELECT payload FROM records")]
            runs = [json.loads(r[0]) for r in db.execute("SELECT payload FROM cooperation_runs")]
        (root / "records.json").write_text(json.dumps(records, indent=2))
        (root / "runs.json").write_text(json.dumps(runs, indent=2))
        assert proc.returncode == 0, str(root)
        assert records and len({(r["session_id"], r["prompt_id"]) for r in records}) == 1, str(root)
        assert all(r["actual_tier"] == ("low" if r.get("cooperation", {}).get("role") == "executor" else "high") for r in records), str(root)
        assert all(r["complete"] and r["kind"] == "inference" for r in records), str(root)
        executor = next(r for r in runs if r["role"] == "executor")
        assert executor["status"] == {"needs-design":"needs_design","document":"needs_parent_write","code":"completed"}[case], str(root)
        if case != "needs-design":
            assert next(r for r in runs if r["role"] == "reviewer")["status"] == "passed", str(root)
            if case == "document":
                assert (work / "report.md").read_text() == "# Report\nRows: 3\nTotal: 6\n", str(root)
            else:
                subprocess.run(["go", "test", "./..."], cwd=work, check=True, capture_output=True)
        print(json.dumps({"case": case, "passed": True, "model_calls": len(records), "executor_calls": state["execution"], "artifacts": str(root)}), flush=True)
    finally:
        if proc is not None and proc.poll() is None:
            os.killpg(proc.pid, signal.SIGTERM)
            proc.wait(timeout=6)
        subprocess.run([str(REPO / "bin/router"), "stop", "-config", str(path), "-force"], env=env, capture_output=True, timeout=10)
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    if not shutil.which("claude") or not (REPO / "bin/router").exists():
        raise SystemExit("Install Claude Code and build bin/router first")
    for case_name in ("document", "code", "needs-design"):
        run_case(case_name)
