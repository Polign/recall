"""A local embedding service for polign mcp -embed-url, backed by OpenAI.

    python embed_proxy.py --port 8765 --model text-embedding-3-small --dim 512

polign posts {"text": ...} to /embed and reads {"values": [...]}. Vectors
are cached in out/embed-cache-<model>-<dim>.sqlite by text hash, so rerunning
a method embeds nothing new. Point the harness at it with
LME_EMBED_URL=http://127.0.0.1:8765.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import sqlite3
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import lme

MAX_CHARS = 24000  # text-embedding-3 takes 8191 tokens; a round can be longer


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--model", default="text-embedding-3-small")
    ap.add_argument("--dim", type=int, default=512)
    args = ap.parse_args()

    from openai import OpenAI
    client = OpenAI()
    db = sqlite3.connect(lme.HERE / "out" / f"embed-cache-{args.model}-{args.dim}.sqlite", check_same_thread=False)
    db.execute("create table if not exists v (h text primary key, values_json text)")
    lock = threading.Lock()

    def embed(text: str) -> list[float]:
        h = hashlib.sha256(text.encode()).hexdigest()
        with lock:
            row = db.execute("select values_json from v where h = ?", (h,)).fetchone()
        if row:
            return json.loads(row[0])
        for attempt in range(8):
            try:
                r = client.embeddings.create(model=args.model, input=text[:MAX_CHARS] or " ", dimensions=args.dim)
                break
            except Exception:
                if attempt == 7:
                    raise
                import time
                time.sleep(2 ** attempt)
        values = r.data[0].embedding
        with lock:
            db.execute("insert or replace into v values (?, ?)", (h, json.dumps(values)))
            db.commit()
        return values

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            try:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                out, status = {"values": embed(body.get("text", ""))}, 200
            except Exception as exc:  # report to polign rather than drop the connection
                out, status = {"error": str(exc)[:300]}, 500
            payload = json.dumps(out).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *a) -> None:
            pass

    print(f"embedding with {args.model} ({args.dim} dims) on :{args.port}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
