"""Exercise the installed-binary setup flow in an isolated directory. No model/API dependency."""
import concurrent.futures
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


cli = str(Path(sys.argv[1]).resolve())
server = str(Path(sys.argv[2]).resolve())
with tempfile.TemporaryDirectory(prefix="recall setup 'test ") as tmp:
    root = Path(tmp)
    env = {k: v for k, v in os.environ.items() if not k.startswith("POLIGN_")}
    env["POLIGN_RECALL_HOME"] = str(root)
    occupied = []
    for port in (23000, 23001):
        sock = socket.socket()
        try:
            sock.bind(("127.0.0.1", port))
            sock.listen()
            occupied.append(sock)
        except OSError:
            sock.close()  # An existing service already exercises the conflict.

    def run(*args, expected=0, extra_env=None):
        result = subprocess.run([cli, *args], env=env | (extra_env or {}), capture_output=True, text=True, timeout=60)
        require(result.returncode == expected, f"{args}: {result.stdout} {result.stderr}")
        return result

    def runtime():
        return json.loads((root / "runtime.json").read_text())

    def stop(sig=signal.SIGTERM):
        if not (root / "runtime.json").exists():
            return
        pid = runtime()["pid"]
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            return
        for _ in range(300):
            if sig == signal.SIGTERM and not (root / "runtime.json").exists():
                return
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                return
            time.sleep(0.1)
        raise RuntimeError("test server did not exit")

    def tool(name, arguments):
        for attempt in range(10):
            requests = [{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18"}},
                        {"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": name, "arguments": arguments}}]
            proc = subprocess.run([str(root / "launch")], input="".join(json.dumps(r) + "\n" for r in requests), env=env,
                                  capture_output=True, text=True, timeout=60)
            require(proc.returncode == 0, f"launcher failed: {proc.stderr}")
            replies = [json.loads(line) for line in proc.stdout.splitlines()]
            reply = next(r for r in replies if r["id"] == 2)
            result = reply.get("result", {})
            text = " ".join(c.get("text", "") for c in result.get("content", []))
            if name in ("recall", "memory_history") and "log changed while materializing; retry" in text:
                time.sleep(0.2)
                continue
            require("error" not in reply and not result.get("isError"), f"{name} failed: {text}")
            return json.loads(text)
        raise RuntimeError("read remained unstable")

    try:
        run("setup", "-local", "-server", server, "-no-plugin")
        first = runtime()
        require(first["url"].startswith("http://127.0.0.1:"), "server is not loopback-only")
        require(not first["url"].endswith((":23000", ":23001")), "setup reused reserved ports")
        for name in ("config.json", "local-key", "runtime.json"):
            require((root / name).stat().st_mode & 0o777 == 0o600, f"{name} is not private")
        try:
            urllib.request.urlopen(first["url"] + "/v1/collections/test/vectors")
            raise RuntimeError("anonymous database access succeeded")
        except urllib.error.HTTPError as error:
            require(error.code == 401, "anonymous access did not return 401")
        run("doctor")
        before = (root / "config.json").read_bytes()
        run("setup", "-no-plugin", extra_env={"POLIGN_URL": "http://127.0.0.1:1", "POLIGN_COLLECTION": "wrong"})
        require((root / "config.json").read_bytes() == before, "rerun changed saved connection")
        require(runtime()["pid"] == first["pid"], "rerun started a second server")
        run("setup", "-url", "http://127.0.0.1:1", "-no-plugin", expected=1)
        require((root / "config.json").read_bytes() == before, "failed setup replaced working configuration")

        # Claude is represented by a recording stub; no account or host plugin
        # settings are touched, and the installation commands remain separate.
        fake = root / "claude"
        record = root / "claude-calls.jsonl"
        fake.write_text(f"#!{sys.executable}\nimport json,sys\nfrom pathlib import Path\nargs=sys.argv[1:]\nwith Path({str(record)!r}).open('a') as f: f.write(json.dumps(args)+'\\n')\nprint('2.1.270 (Claude Code)' if args==['--version'] else '[]' if args[-1:]==['--json'] else 'OK')\n")
        fake.chmod(0o700)
        run("setup", "-claude", str(fake))
        calls = [json.loads(line) for line in record.read_text().splitlines()]
        require(["plugin", "marketplace", "add", "https://github.com/Polign/polign.git"] in calls, "marketplace command missing")
        require(["plugin", "install", "-y", "recall@polign"] in calls, "plugin install command missing")
        require(all("--json" not in call or call[:3] == ["plugin", "marketplace", "list"] for call in calls), "unexpected --json use")

        pair = {"subject": "onboarding-test", "predicate": "prefers_editor"}
        tool("remember", pair | {"value": "vim"})
        tool("remember", pair | {"value": "neovim"})
        require("neovim" in json.dumps(tool("recall", pair)), "fresh process lost corrected memory")
        require(len(tool("memory_history", pair)) == 2, "correction history missing")
        stop(signal.SIGKILL)
        # Concurrent MCP sessions must recover one server from stale runtime
        # metadata, without two writers opening the same local data directory.
        with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
            results = list(pool.map(lambda _: tool("recall", pair), range(3)))
        require(all("neovim" in json.dumps(result) for result in results), "crash recovery lost memory")
        require(runtime()["pid"] != first["pid"], "server did not restart")
        require(len(tool("memory_history", pair)) == 2, "restart lost history")
        run("doctor")

        # The credential record lives inside the data directory and the key
        # beside it. The server registers the key again at every launch, so
        # losing the directory while the key survives is repaired by the next
        # start, with the same key, without rerunning setup.
        stop()
        previous_key = (root / "local-key").read_text()
        shutil.rmtree(root / "data")
        tool("remember", pair | {"value": "vim"})
        require((root / "local-key").read_text() == previous_key, "launcher replaced the local key")
        require("vim" in json.dumps(tool("recall", pair)), "rebuilt database cannot be read")
        run("setup", "-no-plugin")
        require((root / "local-key").read_text() == previous_key, "setup rerun replaced the local key")
        require((root / "local-key").stat().st_mode & 0o777 == 0o600, "local key is not private")
        run("doctor")
        print("PASS: setup, occupied ports, private auth, reruns, failed reconfiguration, Claude commands, correction/history, crash recovery, concurrent sessions, lost data directory")
    finally:
        stop()
        for sock in occupied:
            sock.close()
