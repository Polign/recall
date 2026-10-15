"""Copies every question's recall-server store into a model-embedder store, so
an LME_EMBED_URL run reads the facts an earlier run extracted without
extracting them again.

    python reembed_stores.py --src recall-server-anthropic_claude-sonnet-5-5 --embed-url http://127.0.0.1:8766

For each question under out/stores/<src>, the store is copied to
out/stores/<src>-embed, its polign-server is started, the events in
recall_lexical_v1 are imported into recall_embed_v1 (re-embedded, see
Client.ImportEvents), and the server is stopped. Resumable: a store already
copied is skipped.
"""
from __future__ import annotations

import argparse
import json
import shutil
import subprocess
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

HERE = Path(__file__).resolve().parent
STORES = HERE / "out" / "stores"
SKIP = shutil.ignore_patterns("runtime.json*", "*.lock", "server.log", "launch")


def one(src: Path, dst: Path, recall: str, tool: str, embed_url: str) -> str:
    done = dst / ".reembedded"
    if done.exists():
        return f"{src.name}: already copied"
    if dst.exists():
        shutil.rmtree(dst)
    shutil.copytree(src, dst, ignore=SKIP)
    cfg = json.loads((dst / "config.json").read_text())
    cfg["collection"] = "recall_embed_v1"
    (dst / "config.json").write_text(json.dumps(cfg, indent=2) + "\n")
    subprocess.run([recall, "setup", "-local", "-no-plugin", "-config-dir", str(dst)],
                   check=True, capture_output=True, text=True, timeout=180)
    try:
        url = json.loads((dst / "runtime.json").read_text())["url"]
        out = subprocess.run([tool, "-url", url, "-key-file", str(dst / "local-key"), "-embed-url", embed_url],
                             check=True, capture_output=True, text=True, timeout=1800)
    finally:
        subprocess.run(["pkill", "-f", f"fs:{dst.resolve()}/data"], check=False)
    done.write_text(out.stdout)
    return f"{src.name}: {out.stdout.strip()}"


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", required=True, help="store family under out/stores")
    ap.add_argument("--embed-url", required=True)
    ap.add_argument("--recall", default=str(HERE / ".venv" / "bin" / "recall"))
    ap.add_argument("--tool", default=str(HERE / "out" / "reembed"))
    ap.add_argument("--workers", type=int, default=6)
    args = ap.parse_args()
    src_root, dst_root = STORES / args.src, STORES / f"{args.src}-embed"
    dst_root.mkdir(exist_ok=True)
    todo = sorted(p for p in src_root.iterdir() if (p / "ingest.jsonl").exists())
    print(f"{len(todo)} stores to copy into {dst_root.name}", flush=True)
    failed = 0
    with ThreadPoolExecutor(args.workers) as pool:
        futs = {pool.submit(one, p, dst_root / p.name, args.recall, args.tool, args.embed_url): p for p in todo}
        for n, f in enumerate(as_completed(futs), 1):
            try:
                msg = f.result()
            except Exception as exc:
                failed += 1
                msg = f"{futs[f].name}: FAILED {exc}"
                if isinstance(exc, subprocess.CalledProcessError):
                    msg += f" {(exc.stderr or '')[:300]}"
            print(f"{n}/{len(todo)} {msg}", flush=True)
    print(f"done, {failed} failed", flush=True)


if __name__ == "__main__":
    main()
