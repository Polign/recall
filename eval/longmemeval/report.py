"""Summarize one or more runs: QA accuracy by question type, and session
retrieval quality for the methods that retrieve.

    python report.py full-4o oracle-4o bm25s-k10

Accuracy counts abstention questions inside their own type (as upstream
does) and also reports them on their own line. Retrieval metrics skip
abstention questions, whose answer sessions do not hold the answer.
"""
from __future__ import annotations

import argparse
import json
import math
from collections import defaultdict

import lme

TYPES = ("single-session-user", "single-session-assistant", "single-session-preference",
         "multi-session", "knowledge-update", "temporal-reasoning")
KS = (5, 10)
EXCLUDE: set[str] = set()


def accuracy(run: str) -> dict[str, tuple[float, int]]:
    rows = [r for r in lme.read_jsonl(lme.HERE / "out" / run / "judge.jsonl") if r["question_id"] not in EXCLUDE]
    groups: dict[str, list[bool]] = defaultdict(list)
    for r in rows:
        groups[r["question_type"]].append(r["label"])
        groups["all"].append(r["label"])
        if r["abstention"]:
            groups["abstention"].append(r["label"])
    return {k: (sum(v) / len(v), len(v)) for k, v in groups.items()}


def retrieval(run: str, refs: dict[str, dict]) -> dict[str, float]:
    manifest = json.loads((lme.HERE / "out" / run / "manifest.json").read_text())
    if not manifest["method"].startswith(("bm25", "recall-")):
        return {}
    sums: dict[str, float] = defaultdict(float)
    n = 0
    for h in lme.read_jsonl(lme.HERE / "out" / run / "hyp.jsonl"):
        if lme.is_abstention(h["question_id"]) or h["question_id"] in EXCLUDE:
            continue
        gold = set(refs[h["question_id"]]["answer_session_ids"])
        ranked = h["retrieved_session_ids"]
        n += 1
        for k in KS:
            top = ranked[:k]
            hits = gold & set(top)
            sums[f"recall_any@{k}"] += bool(hits)
            sums[f"recall_all@{k}"] += hits == gold
            dcg = sum(1 / math.log2(i + 2) for i, s in enumerate(top) if s in gold)
            idcg = sum(1 / math.log2(i + 2) for i in range(min(k, len(gold))))
            sums[f"ndcg@{k}"] += dcg / idcg if idcg else 0.0
    return {k: v / n for k, v in sums.items()} if n else {}


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("runs", nargs="+")
    ap.add_argument("--data", default="longmemeval_s_cleaned.json")
    ap.add_argument("--subset", choices=tuple(lme.SUBSETS), default="all",
                    help="score only these questions (see run.py --subset)")
    ap.add_argument("--holdout", type=int, default=0,
                    help="leave out the stratified sample of this many questions used for tuning (run.py --limit)")
    args = ap.parse_args()
    entries = lme.load(args.data)
    refs = {e["question_id"]: e for e in entries}
    global EXCLUDE
    EXCLUDE = {e["question_id"] for e in lme.sample(entries, args.holdout)} if args.holdout else set()
    EXCLUDE |= {e["question_id"] for e in entries if not lme.SUBSETS[args.subset](e)}
    if args.subset != "all":
        print(f"Subset {args.subset}: {len(entries) - len(EXCLUDE)} questions scored.\n")
    elif EXCLUDE:
        print(f"Held out: the {len(EXCLUDE)} tuning questions are left out.\n")

    accs = {r: accuracy(r) for r in args.runs}
    width = max(12, *(len(r) for r in args.runs))
    print("QA accuracy (judged / n)".ljust(28) + "".join(r.rjust(width + 2) for r in args.runs))
    for key in ("all", *TYPES, "abstention"):
        if not any(key in accs[r] for r in args.runs):
            continue
        cells = []
        for r in args.runs:
            acc, n = accs[r].get(key, (float("nan"), 0))
            cells.append(f"{acc * 100:5.1f} ({n})".rjust(width + 2))
        print(key.ljust(28) + "".join(cells))

    rets = {r: retrieval(r, refs) for r in args.runs}
    if any(rets.values()):
        print("\nSession retrieval (non-abstention)")
        keys = [f"{m}@{k}" for k in KS for m in ("recall_any", "recall_all", "ndcg")]
        for key in keys:
            cells = [(f"{rets[r][key] * 100:5.1f}" if key in rets[r] else "-").rjust(width + 2) for r in args.runs]
            print(key.ljust(28) + "".join(cells))


if __name__ == "__main__":
    main()
