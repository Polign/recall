#!/bin/bash
# The full LongMemEval-S run on the published packages: every question, four
# setups, gpt-4o answering and judging. Resumable: rerun to finish a stopped run.
#   bash run500.sh [polign-recall version]
set -euo pipefail
cd "$(dirname "$0")"
version="${1:-0.6.0}"
venv=/tmp/lme-release-$version

if [ ! -x "$venv/bin/python" ]; then
  python3 -m venv "$venv"
  "$venv/bin/pip" install -q "polign-recall==$version" openai tiktoken
fi
unset LME_POLIGN_BIN
export OPENAI_API_KEY="${OPENAI_API_KEY:-$(bash -c 'source ~/.bashrc >/dev/null 2>&1; printf %s "$OPENAI_API_KEY"')}"
PY="$venv/bin/python"
"$PY" -c "import polign_recall, polign_db, importlib.metadata as m; print('polign-recall', polign_recall.__version__, '| polign_db', m.version('polign_db'))"

for spec in "oracle-4o-500 oracle 16" "bm25t-4o-500 bm25-turn 16" "notes-4o-500 recall-notes 10" "linked-4o-500 recall-linked 10"; do
  set -- $spec
  until "$PY" run.py --run "$1" --method "$2" --k 10 --workers "$3"; do echo "retrying failed questions in $1"; sleep 30; done
  "$PY" judge.py --run "$1" --workers 16
done

runs="oracle-4o-500 bm25t-4o-500 notes-4o-500 linked-4o-500"
"$PY" report.py $runs | tee out/report-500.txt
"$PY" report.py $runs --holdout 50 | tee out/report-450-holdout.txt
