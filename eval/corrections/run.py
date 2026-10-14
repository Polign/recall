"""Corrections across predicates, measured on small hand-written cases.

Each case says one thing, then later something else. In a correction case
the second statement makes the first untrue under another predicate ("I love
mangoes", then "I can't stand mangoes anymore"), and the first must be gone.
In a trap the second is related but leaves the first true ("I also like
papayas"), and the first must remain. The check is on the exact values the
setup filed: a correction passes when none of them is still held, a trap
when all of them are. A case whose first statement was never extracted
counts as neither.

Everything goes through the product path: a text-mode Recall server with
-extract-model, the personal starter registry, one collection per case.

    python run.py --model ollama:qwen3:8b            # free, slow
    python run.py --model openai:gpt-4o-mini-2024-07-18
    python run.py --model ... --only mango,t-mango   # a few cases

Needs a polign-server and a recall binary on PATH (or RECALL_BIN), and the
polign_recall package from ../../python.
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
REGISTRY = HERE.parent.parent / "registries" / "personal.json"


def start_server(store: Path, port: int) -> tuple[subprocess.Popen, str]:
    key = "plgn_" + os.urandom(8).hex() + "_" + os.urandom(32).hex()
    (store / "key").write_text(key)
    proc = subprocess.Popen(["polign-server", "-store", f"fs:{store}/data", "-require-data-key",
                             "-bootstrap-key-file", str(store / "key"), "-http", f"127.0.0.1:{port}",
                             "-grpc", f"127.0.0.1:{port + 1}", "-telemetry=false"],
                            stdout=subprocess.DEVNULL, stderr=open(store / "server.log", "w"))
    for _ in range(100):
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1)
            return proc, key
        except Exception:
            time.sleep(0.2)
    proc.kill()
    raise SystemExit("polign-server did not start; see " + str(store / "server.log"))


def held(client, preds: list[str], word: str) -> list[str]:
    """Current beliefs about the user under preds whose value has word."""
    out = []
    for p in preds:
        for b in client.recall(subject="user", predicate=p):
            if word.lower() in str(b.value).lower():
                out.append(f"{p}: {b.value}")
    return out


def run_case(case: dict, env: dict, model: str) -> dict:
    from polign_recall import Client
    env = {**env, "POLIGN_COLLECTION": "case_" + case["id"].replace("-", "_")}
    checks = case.get("gone") or case.get("kept")
    talk = Client(env=env, text=True, extract_model=model, timeout=600)
    look = Client(env=env, typed=True, timeout=120)   # recall 0.18+ serves the text tools by default
    try:
        talk.remember(text=case["setup"])
        before = [held(look, c["predicates"], c["word"]) for c in checks]
        if not all(before):
            return {"id": case["id"], "kind": case["kind"], "result": "setup-missed", "before": before}
        said = talk.remember(text=case["say"])
        # Everything now held under the checked predicates; the old values
        # must be gone (correction) or all still there (trap).
        after = [held(look, c["predicates"], "") for c in checks]
        if case["kind"] == "correction":
            ok = not any(v in now for old, now in zip(before, after) for v in old)
        else:
            ok = all(v in now for old, now in zip(before, after) for v in old)
        return {"id": case["id"], "kind": case["kind"], "result": "pass" if ok else "fail",
                "before": before, "after": after,
                "filed": [{"text": r.text, "replaces": list(r.replaces)} for r in said.remembered]}
    finally:
        talk.close()
        look.close()


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True, help="provider:model for extraction and correction")
    ap.add_argument("--only", help="comma-separated case ids")
    ap.add_argument("--out", help="results file (default out/<model>.jsonl)")
    ap.add_argument("--port", type=int, default=23495, help="HTTP port for this run's polign-server (gRPC is port+1)")
    a = ap.parse_args()

    cases = json.loads((HERE / "cases.json").read_text())
    if a.only:
        keep = set(a.only.split(","))
        cases = [c for c in cases if c["id"] in keep]
    out = Path(a.out or HERE / "out" / (a.model.replace(":", "_").replace("/", "_") + ".jsonl"))
    out.parent.mkdir(exist_ok=True)
    done = {json.loads(l)["id"] for l in out.read_text().splitlines()} if out.exists() else set()

    store = Path(tempfile.mkdtemp(prefix="recall-corrections-"))
    proc, key = start_server(store, a.port)
    env = {"POLIGN_URL": f"http://127.0.0.1:{a.port}", "POLIGN_API_KEY": key, "POLIGN_PREDICATES": str(REGISTRY)}
    try:
        for case in cases:
            if case["id"] in done:
                continue
            t0 = time.monotonic()
            try:
                r = run_case(case, env, a.model)
            except Exception as exc:  # one broken case must not end the run
                r = {"id": case["id"], "kind": case["kind"], "result": "error", "error": str(exc)[:500]}
            r["seconds"] = round(time.monotonic() - t0, 1)
            with out.open("a") as f:
                f.write(json.dumps(r) + "\n")
            print(f"{r['result']:13} {case['kind']:10} {case['id']:14} {r['seconds']:6}s", flush=True)
    finally:
        proc.terminate()
        proc.wait(timeout=30)
        shutil.rmtree(store, ignore_errors=True)
    report(out)


def report(path: Path) -> None:
    rows = [json.loads(l) for l in path.read_text().splitlines()]
    for kind, label in (("correction", "corrections made"), ("trap", "traps left alone")):
        mine = [r for r in rows if r["kind"] == kind]
        valid = [r for r in mine if r["result"] in ("pass", "fail")]
        passed = sum(r["result"] == "pass" for r in valid)
        print(f"{label}: {passed}/{len(valid)}"
              f" (setup missed {sum(r['result'] == 'setup-missed' for r in mine)},"
              f" errors {sum(r['result'] == 'error' for r in mine)})")
    for r in rows:
        if r["result"] == "fail":
            print(f"  FAIL {r['id']}: before {r['before']} after {r['after']} filed {r.get('filed')}")


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "report":
        report(Path(sys.argv[2]))
    else:
        main()
