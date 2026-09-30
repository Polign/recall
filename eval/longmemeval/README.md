# LongMemEval harness

Runs [LongMemEval](https://github.com/xiaowu0162/LongMemEval) (the cleaned
2025/09 release, `_S` split: 500 questions, about 50 sessions and 104k tokens
of chat history each) against baseline methods, and later against Recall.

The reader prompt, history format, history truncation, and judge prompts are
copied from upstream, so baseline numbers can be checked against the paper
before any Recall number is trusted.

## Setup

```sh
./fetch.sh                                   # about 290 MB into data/
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
export OPENAI_API_KEY=...                    # judge, and the default reader
```

## Run

```sh
# generate hypotheses (resumable: rerun the same --run to continue)
.venv/bin/python run.py --run full-4o   --method full
.venv/bin/python run.py --run oracle-4o --method oracle
.venv/bin/python run.py --run bm25s-k10 --method bm25-session --k 10
.venv/bin/python run.py --run bm25t-k10 --method bm25-turn    --k 10

# judge each run, then compare them side by side
.venv/bin/python judge.py --run full-4o
.venv/bin/python report.py full-4o oracle-4o bm25s-k10 bm25t-k10
```

Useful flags on `run.py`:

- `--limit 50` picks a stratified subset (same questions every time), which
  is good for iterating before paying for all 500.
- `--reader anthropic:<model>` swaps the reader. Keep the judge on
  `gpt-4o-2024-08-06`, or the numbers are not comparable with published ones.
- `--cot` uses upstream's step-by-step reader prompt.
- `--dry-run` builds retrieval and prompts without calling a model, so the
  retrieval metrics in `report.py` are free.

## Methods

| Method | What the reader sees |
|---|---|
| `full` | every session in the haystack |
| `oracle` | only the sessions that hold the answer (the ceiling for any retriever) |
| `bm25-session` | the top `k` sessions by BM25 on the question |
| `bm25-turn` | the top `k` rounds (a user turn plus the reply after it) |

## Output

`out/<run>/` holds `manifest.json` (arguments and the data file's hash),
`hyp.jsonl` (hypothesis and retrieved session ids per question) and
`judge.jsonl` (the judge's verdict per question).

`report.py` prints accuracy per question type. Abstention questions are
counted inside their type, as upstream does, and also on a line of their
own. It also prints session recall and NDCG at 5 and 10 for the retrieval
methods, leaving out abstention questions.
