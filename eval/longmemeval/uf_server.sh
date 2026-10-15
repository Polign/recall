#!/bin/bash
# Recall through its own extractor on the 267 user-facts questions, beside the
# oracle and BM25-session baselines, all on one model as reader, judge and
# extractor. gpt-4o, the upstream reader and judge, is being retired, so these
# are the anchors from here on; the gpt-4o-era runs are not comparable.
#
# On 2026-10-14 with anthropic:claude-sonnet-5-5: oracle 96.6, BM25 89.9,
# recall-server 88.8; about $30 for the baselines and $180 for the Recall run
# (267 questions, about 40 extraction calls each). Resumable: rerun to finish.
#
#   bash uf_server.sh                       # Sonnet 5.5
#   MODEL=anthropic:claude-opus-5-5 bash uf_server.sh
set -euo pipefail
cd "$(dirname "$0")"
MODEL="${MODEL:-anthropic:claude-sonnet-5-5}"
TAG="${TAG:-sonnet}"
: "${ANTHROPIC_API_KEY:=$(bash -c 'source ~/.bashrc >/dev/null 2>&1; printf %s "$ANTHROPIC_API_KEY"')}"
export ANTHROPIC_API_KEY
[ -n "$ANTHROPIC_API_KEY" ] || { echo "ANTHROPIC_API_KEY is not set" >&2; exit 1; }
export LME_RECALL_BIN="${LME_RECALL_BIN:-$PWD/.venv/bin/recall}" LME_STORES="$PWD/out/stores"
PY=.venv/bin/python

"$PY" run.py --run "uf-oracle-$TAG" --method oracle --subset user-facts --reader "$MODEL" --workers 6
"$PY" judge.py --run "uf-oracle-$TAG" --judge "$MODEL" --workers 8
"$PY" run.py --run "uf-bm25s-$TAG" --method bm25-session --k 10 --subset user-facts --reader "$MODEL" --workers 6
"$PY" judge.py --run "uf-bm25s-$TAG" --judge "$MODEL" --workers 8
"$PY" run.py --run "uf-server-$TAG" --method recall-server --extractor "$MODEL" --reader "$MODEL" --subset user-facts --ages --workers 6
"$PY" judge.py --run "uf-server-$TAG" --judge "$MODEL" --workers 8
"$PY" report.py "uf-oracle-$TAG" "uf-bm25s-$TAG" "uf-server-$TAG" | tee "out/report-uf-server-$TAG.txt"
