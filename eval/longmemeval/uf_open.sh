#!/bin/bash
# Open vocabulary against the hand-tuned registry on all 267 questions of the
# user-facts subset, all on OpenAI: gpt-4o-mini extraction with the same
# prompt rules, gpt-4o answering and judging.
#
# The fixed version is uf-rel012-ages (gpt-4o, same settings), copied without
# new calls: this branch retrieves the same sessions for all 267 questions
# (checked with a dry run on 2026-10-10). The open version starts from the 77
# knowledge-update questions ku_open.sh already answered and judged, so only
# the other 190 are paid for: gpt-4o-mini extraction of about 9,100 sessions
# (about $6) and gpt-4o answering and judging (about $2.70), about $8.70 at
# list prices. Resumable: rerun to finish a stopped run.
#
#   bash uf_open.sh
set -euo pipefail
cd "$(dirname "$0")"
: "${OPENAI_API_KEY:=$(bash -c 'source ~/.bashrc >/dev/null 2>&1; printf %s "$OPENAI_API_KEY"')}"
export OPENAI_API_KEY
[ -n "$OPENAI_API_KEY" ] || { echo "OPENAI_API_KEY is not set" >&2; exit 1; }

(cd ../.. && go build -o eval/longmemeval/out/recall-branch ./cmd/recall)
export LME_RECALL_BIN="$PWD/out/recall-branch" LME_STORES="$PWD/out/stores"
PY=.venv/bin/python

"$PY" - <<'EOF'
import json, shutil
import lme
out = lme.HERE / "out"
fixed = out / "uf-fixed-4o"
fixed.mkdir(exist_ok=True)
for name in ("hyp.jsonl", "judge.jsonl"):
    shutil.copy(out / "uf-rel012-ages" / name, fixed / name)
manifest = json.loads((out / "uf-rel012-ages" / "manifest.json").read_text())
manifest.update(run="uf-fixed-4o", copied_from="uf-rel012-ages")
(fixed / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")

# The 77 knowledge-update questions already answered and judged by
# ku_open.sh, with the same settings, are carried over rather than redone.
open_ = out / "uf-open-4o"
open_.mkdir(exist_ok=True)
for name in ("hyp.jsonl", "judge.jsonl"):
    have = {r["question_id"] for r in lme.read_jsonl(open_ / name)}
    for r in lme.read_jsonl(out / "ku-open-4o" / name):
        if r["question_id"] not in have:
            lme.append_jsonl(open_ / name, r)
print("fixed:", len(lme.read_jsonl(fixed / "judge.jsonl")), "judged;",
      "open:", len(lme.read_jsonl(open_ / "hyp.jsonl")), "answered so far")
EOF

LME_OPEN_VOCAB=1 "$PY" run.py --run uf-open-4o --subset user-facts --method recall-linked --ages --workers 6
"$PY" judge.py --run uf-open-4o --workers 8
"$PY" report.py uf-fixed-4o uf-open-4o | tee out/report-uf-open.txt
