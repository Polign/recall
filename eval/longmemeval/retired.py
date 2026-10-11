"""What a model-driven corrector retired, with the words that made it do so.

The ku_correct.sh runs on the at/corrective-memory branch let gpt-4o-mini
say which existing beliefs each new statement made untrue. Every belief it
retired is a retraction with replaced_by in its store. This lists each one
beside the statement that retired it, that statement's evidence (the words
the model judged by), and the user turn the evidence came from, so each
retirement can be checked by hand against the conversation.

    python retired.py                # both runs, to out/retired.jsonl
    python retired.py --stores recall-linked-gpt-4o-mini-2024-07-18-open-correct

Needs the stores under out/stores (LME_STORES at the time of the run) and a
recall binary that reads them.
"""
from __future__ import annotations

import argparse
import json
import sys
import urllib.parse
import urllib.request

import lme
import recall_method
from polign_recall.client import Client

REGISTRY = lme.HERE.parent.parent / "registries" / "personal.json"
RUNS = {
    "recall-linked-gpt-4o-mini-2024-07-18-correct": ("ku-correct-4o", False),
    "recall-linked-gpt-4o-mini-2024-07-18-open-correct": ("ku-open-correct-4o", True),
}


def turn_for(entry: dict, evidence: str) -> tuple[str, str]:
    """(session date, user turn) holding the evidence words, or ("", "")."""
    needle = " ".join(evidence.split()).lower()
    for sid, date, turns in lme.sessions(entry):
        for t in turns:
            if t["role"] == "user" and needle[:60] in " ".join(t["content"].split()).lower():
                return date, t["content"]
    return "", ""


def retired(store, open_vocab: bool) -> list[dict]:
    env = {"POLIGN_PREDICATES": str(REGISTRY if open_vocab else lme.HERE / "registry.json")}
    if open_vocab:
        env["RECALL_OPEN"] = "1"
    out = []
    with Client(local_dir=store, write=False, timeout=120, env=env):
        url = json.loads((store / "runtime.json").read_text())["url"]
        key = (store / "local-key").read_text().strip()
        base = f"{url}/v1/collections/{recall_method.COLLECTION}/vectors"
        headers = {"Authorization": f"Bearer {key}"}
        q = urllib.parse.urlencode({"filter": json.dumps({"replaced_by": {"$exists": True}}), "typed": "true", "limit": 1000})
        rows = json.loads(urllib.request.urlopen(urllib.request.Request(f"{base}?{q}", headers=headers)).read())["vectors"]
        for r in rows:
            m = r["metadata"]
            rep = json.loads(urllib.request.urlopen(urllib.request.Request(f"{base}/{m['replaced_by']}", headers=headers)).read())["metadata"]
            out.append({
                "retired": {"predicate": m["predicate"], "value": m["value"], "stated": m["observed_at"][:10]},
                "by": {"predicate": rep["predicate"], "value": rep["value"], "stated": rep["observed_at"][:10]},
                "evidence": rep.get("evidence", ""),
            })
    recall_method.stop_local_server(store)
    return out


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--stores", nargs="*", default=list(RUNS))
    ap.add_argument("--out", default=str(lme.HERE / "out" / "retired.jsonl"))
    args = ap.parse_args()
    entries = {e["question_id"]: e for e in lme.load()}
    rows = []
    for name in args.stores:
        run, open_vocab = RUNS[name]
        root = lme.HERE / "out" / "stores" / name
        stores = sorted(p for p in root.iterdir() if (p / "ingest.jsonl").exists())
        n = 0
        for store in stores:
            for item in retired(store, open_vocab):
                date, turn = turn_for(entries[store.name], item["evidence"])
                rows.append({"run": run, "question_id": store.name, **item, "turn_date": date, "turn": turn})
                n += 1
            print(f"\r{name}: {stores.index(store) + 1}/{len(stores)} stores, {n} retired", end="", file=sys.stderr)
        print(file=sys.stderr)
    with open(args.out, "w") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")
    print(len(rows), "retirements written to", args.out)


if __name__ == "__main__":
    main()
