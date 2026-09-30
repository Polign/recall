#!/bin/sh
# 50-question stratified pass: every method, then the judge, then the report.
set -u
cd "$(dirname "$0")"
PY=.venv/bin/python
for spec in "oracle-4o-50 oracle" "bm25s-4o-50 bm25-session" "bm25t-4o-50 bm25-turn" "notes-4o-50 recall-notes" "full-4o-50 full"; do
  set -- $spec
  workers=8; [ "$2" = full ] && workers=3
  $PY run.py --run "$1" --method "$2" --k 10 --limit 50 --workers $workers || echo "RUN FAILED: $1"
  $PY judge.py --run "$1" || echo "JUDGE FAILED: $1"
done
$PY report.py oracle-4o-50 full-4o-50 bm25s-4o-50 bm25t-4o-50 notes-4o-50
