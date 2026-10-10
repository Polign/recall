#!/bin/bash
# Open vocabulary against the hand-tuned registry, on the 77 knowledge-update
# questions of the user-facts subset, all on OpenAI: gpt-4o-mini extraction
# with the same prompt rules, gpt-4o answering and judging. The difference
# between the two versions is the vocabulary.
#
# The fixed version is the knowledge-update rows of uf-rel012-ages (gpt-4o,
# same settings), copied here without new calls: this branch retrieves the
# same sessions for all 77 questions (checked with a dry run on 2026-10-10).
# Only the open version is paid for: gpt-4o-mini extraction of 3,655
# sessions (about $2.40) and gpt-4o answering and judging of 77 questions
# (about $1.05), about $3.50 at list prices. Resumable: rerun to finish a
# stopped run.
#
#   bash ku_open.sh      # all 77 questions, then judge and report
#   bash ku_open.sh 5    # pilot: the first 5 questions only, then show the
#                        # predicates coined; the full run reuses its work
set -euo pipefail
pilot="${1:-}"
cd "$(dirname "$0")"
: "${OPENAI_API_KEY:=$(bash -c 'source ~/.bashrc >/dev/null 2>&1; printf %s "$OPENAI_API_KEY"')}"
export OPENAI_API_KEY
[ -n "$OPENAI_API_KEY" ] || { echo "OPENAI_API_KEY is not set" >&2; exit 1; }

# The open version needs this branch's recall (RECALL_OPEN).
(cd ../.. && go build -o eval/longmemeval/out/recall-branch ./cmd/recall)
export LME_RECALL_BIN="$PWD/out/recall-branch" LME_STORES="$PWD/out/stores"
PY=.venv/bin/python

"$PY" - <<'EOF'
import json, shutil
import lme
ku = [e["question_id"] for e in lme.subset(lme.load(), "user-facts") if e["question_type"] == "knowledge-update"]
open("out/ku-ids.txt", "w").write("\n".join(ku) + "\n")
src, dst = lme.HERE / "out" / "uf-rel012-ages", lme.HERE / "out" / "ku-fixed-4o"
dst.mkdir(exist_ok=True)
for name in ("hyp.jsonl", "judge.jsonl"):
    rows = [r for r in lme.read_jsonl(src / name) if r["question_id"] in set(ku)]
    (dst / name).write_text("".join(json.dumps(r) + "\n" for r in rows))
manifest = json.loads((src / "manifest.json").read_text())
manifest.update(run="ku-fixed-4o", only="out/ku-ids.txt", questions=len(ku), copied_from="uf-rel012-ages")
(dst / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
print(len(ku), "knowledge-update questions; fixed version copied from uf-rel012-ages")
EOF

ids=out/ku-ids.txt
if [ -n "$pilot" ]; then
  head -n "$pilot" out/ku-ids.txt > out/ku-ids-pilot.txt
  ids=out/ku-ids-pilot.txt
fi
LME_OPEN_VOCAB=1 "$PY" run.py --run ku-open-4o --subset user-facts --only "$ids" --method recall-linked --ages --workers 6
if [ -n "$pilot" ]; then
  LME_OPEN_VOCAB=1 "$PY" - <<'EOF2'
import collections, json
import extract, lme
seeds = extract.seeds()
counts, coined = collections.Counter(), {}
for f in (lme.HERE / "out" / "extract-cache" / extract.cache_key("gpt-4o-mini-2024-07-18")).glob("*.jsonl"):
    for row in lme.read_jsonl(f):
        for st in row["statements"]:
            counts[st["predicate"]] += 1
            if st["predicate"] not in seeds:
                coined.setdefault(st["predicate"], st.get("cardinality") or "single")
total = sum(counts.values())
print(f"{total} statements, {len(counts)} predicates, {len(coined)} coined; "
      f"{sum(c for p, c in counts.items() if p in seeds) / max(total, 1):.0%} of statements use a seed")
for p, c in counts.most_common(30):
    print(f"  {c:4d} {p}" + ("" if p in seeds else f"  (coined, {coined[p]})"))
EOF2
  exit 0
fi
"$PY" judge.py --run ku-open-4o --workers 8
"$PY" report.py ku-fixed-4o ku-open-4o | tee out/report-ku-open.txt
