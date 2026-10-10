#!/bin/bash
# Open vocabulary against the hand-tuned registry, on the 77 knowledge-update
# questions of the user-facts subset. Both versions use the same gpt-4o-mini
# extraction prompt rules and the same reader (Claude Sonnet 5.5), and gpt-4o
# judges, so the difference between them is the vocabulary.
#
# The fixed version reuses its cached extraction and stores, so it pays only
# for answering and judging. The open version pays for gpt-4o-mini extraction
# of 3,655 sessions as well. Estimated cost, at list prices: about $2.40 of
# OpenAI extraction, about $1.50 of Claude answering, and well under $1 of
# OpenAI judging. Resumable: rerun to finish a stopped run.
#
#   OPENAI_API_KEY=... ANTHROPIC_API_KEY=... bash ku_open.sh
set -euo pipefail
cd "$(dirname "$0")"
: "${OPENAI_API_KEY:=$(bash -c 'source ~/.bashrc >/dev/null 2>&1; printf %s "$OPENAI_API_KEY"')}"
: "${ANTHROPIC_API_KEY:=$(bash -c 'source ~/.bashrc >/dev/null 2>&1; printf %s "$ANTHROPIC_API_KEY"')}"
export OPENAI_API_KEY ANTHROPIC_API_KEY
[ -n "$OPENAI_API_KEY" ] || { echo "OPENAI_API_KEY is not set" >&2; exit 1; }
[ -n "$ANTHROPIC_API_KEY" ] || { echo "ANTHROPIC_API_KEY is not set (use a key from the organization holding the credits)" >&2; exit 1; }

# The open version needs this branch's recall (RECALL_OPEN); both use it, so
# the read side is the same.
(cd ../.. && go build -o eval/longmemeval/out/recall-branch ./cmd/recall)
export LME_RECALL_BIN="$PWD/out/recall-branch" LME_STORES=out/stores
PY=.venv/bin/python
"$PY" -c "
import lme
ids = [e['question_id'] for e in lme.subset(lme.load(), 'user-facts') if e['question_type'] == 'knowledge-update']
open('out/ku-ids.txt', 'w').write('\n'.join(ids) + '\n')
print(len(ids), 'knowledge-update questions')"

common=(--subset user-facts --only out/ku-ids.txt --method recall-linked --ages --reader anthropic:claude-sonnet-5-5 --workers 6)

# The fixed version first: it costs only answering, so a reader problem
# shows up before any extraction is paid for.
"$PY" run.py --run ku-fixed-s55 "${common[@]}"
LME_OPEN_VOCAB=1 "$PY" run.py --run ku-open-s55 "${common[@]}"

"$PY" judge.py --run ku-fixed-s55 --workers 8
"$PY" judge.py --run ku-open-s55 --workers 8
"$PY" report.py ku-fixed-s55 ku-open-s55 | tee out/report-ku-open.txt
