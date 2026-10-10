"""Shared pieces of the LongMemEval harness: data loading, the upstream
reader prompt and history format, the model call, and resumable JSONL output.

The prompt templates and the history format are copied from
xiaowu0162/LongMemEval src/generation/run_generation.py so that the
baselines here can be checked against the paper's numbers.
"""
from __future__ import annotations

import hashlib
import json
import math
import random
import re
import time
from collections import defaultdict
from pathlib import Path
from typing import Any, Iterable

HERE = Path(__file__).resolve().parent
DATA = HERE / "data"

READER_PROMPT = (
    "I will give you several history chats between you and a user. Please answer the question "
    "based on the relevant chat history.\n\n\nHistory Chats:\n\n{}\n\nCurrent Date: {}\nQuestion: {}\nAnswer:"
)
# Upstream's reader prompt for extracted facts (merge_key_expansion_into_value
# = replace), used when the reader sees memory beliefs instead of chat turns.
READER_PROMPT_FACTS = (
    "I will give you several facts extracted from history chats between you and a user. Please answer the "
    "question based on the relevant facts.\n\n\nHistory Chats:\n\n{}\n\nCurrent Date: {}\nQuestion: {}\nAnswer:"
)
READER_PROMPT_FACTS_COT = (
    "I will give you several facts extracted from history chats between you and a user. Please answer the "
    "question based on the relevant facts. Answer the question step by step: first extract all the relevant "
    "information, and then reason over the information to get the answer.\n\n\nHistory Chats:\n\n{}\n\n"
    "Current Date: {}\nQuestion: {}\nAnswer (step by step):"
)
# Upstream's reader prompt for chat history plus extracted facts
# (merge_key_expansion_into_value = merge), used when the reader sees memory
# facts together with the conversation they came from.
READER_PROMPT_MERGE = (
    "I will give you several history chats between you and a user, as well as the relevant user facts "
    "extracted from the chat history. Please answer the question based on the relevant chat history and the "
    "user facts\n\n\nHistory Chats:\n\n{}\n\nCurrent Date: {}\nQuestion: {}\nAnswer:"
)
READER_PROMPT_MERGE_COT = (
    "I will give you several history chats between you and a user, as well as the relevant user facts "
    "extracted from the chat history. Please answer the question based on the relevant chat history and the "
    "user facts. Answer the question step by step: first extract all the relevant information, and then "
    "reason over the information to get the answer.\n\n\nHistory Chats:\n\n{}\n\nCurrent Date: {}\n"
    "Question: {}\nAnswer (step by step):"
)
READER_PROMPT_COT = (
    "I will give you several history chats between you and a user. Please answer the question "
    "based on the relevant chat history. Answer the question step by step: first extract all the "
    "relevant information, and then reason over the information to get the answer.\n\n\n"
    "History Chats:\n\n{}\n\nCurrent Date: {}\nQuestion: {}\nAnswer (step by step):"
)


def load(name: str = "longmemeval_s_cleaned.json") -> list[dict[str, Any]]:
    path = DATA / name
    if not path.exists():
        raise SystemExit(f"{path} is missing. Run ./fetch.sh first.")
    return json.loads(path.read_text())


def file_sha256(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def is_abstention(qid: str) -> bool:
    return "_abs" in qid


# Question types that ask about the user: what they said, prefer, changed,
# and when. Left out: single-session-assistant (asks what the assistant
# said, which Recall does not store as beliefs) and multi-session
# (aggregation across sessions, reported on the full set).
USER_FACT_TYPES = ("single-session-user", "single-session-preference",
                   "knowledge-update", "temporal-reasoning")


def _when(date: str) -> str:
    # "2023/05/20 (Sat) 02:21" -> "2023/05/20 02:21", which sorts by time
    return re.sub(r" \(\w+\)", "", date)


def answer_after_question(entry: dict[str, Any]) -> bool:
    """True when a session holding the answer is dated after the question.
    44 questions in the S split do this (43 temporal-reasoning); a memory
    asked as of the question date correctly cannot see the answer."""
    dates = dict(zip(entry["haystack_session_ids"], entry["haystack_dates"]))
    asked = _when(entry["question_date"])
    return any(_when(dates[s]) > asked for s in entry["answer_session_ids"] if s in dates)


SUBSETS = {
    "all": lambda e: True,
    "valid": lambda e: not answer_after_question(e),
    "user-facts": lambda e: e["question_type"] in USER_FACT_TYPES and not answer_after_question(e),
}


def subset(entries: list[dict[str, Any]], name: str) -> list[dict[str, Any]]:
    return [e for e in entries if SUBSETS[name](e)]


def sample(entries: list[dict[str, Any]], limit: int | None) -> list[dict[str, Any]]:
    """A stratified subset: each question type keeps its share of `limit`,
    and the choice within a type is a stable hash of the question id, so the
    same limit always picks the same questions."""
    if not limit or limit >= len(entries):
        return entries
    by_type: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for e in entries:
        by_type[e["question_type"]].append(e)
    picked = []
    for qtype, group in sorted(by_type.items()):
        group.sort(key=lambda e: hashlib.sha1(e["question_id"].encode()).hexdigest())
        picked += group[: max(1, math.ceil(limit * len(group) / len(entries)))]
    return picked[:limit] if len(picked) > limit else picked


def sessions(entry: dict[str, Any]) -> list[tuple[str, str, list[dict[str, Any]]]]:
    """(session_id, date, turns) in date order, with has_answer stripped so
    the reader never sees the label."""
    out = []
    for sid, date, turns in zip(entry["haystack_session_ids"], entry["haystack_dates"], entry["haystack_sessions"]):
        out.append((sid, date, [{"role": t["role"], "content": t["content"]} for t in turns]))
    out.sort(key=lambda s: s[1])
    return out


def days_before(date: str, now: str) -> int:
    """Whole days from a dataset date to a later one."""
    from datetime import datetime
    day = lambda d: datetime.strptime(d[:10], "%Y/%m/%d")
    return (day(now) - day(date)).days


def format_history(chunks: Iterable[tuple[str, list[dict[str, Any]]]], now: str | None = None) -> str:
    """Upstream's 'nl' history format. Each chunk is (date, turns); chunks are
    sorted by date before numbering, as upstream does. With now (the question
    date), each session date also says how many days before it falls, so the
    reader does not have to do calendar arithmetic."""
    history = ""
    for i, (date, turns) in enumerate(sorted(chunks, key=lambda c: c[0])):
        body = "".join("\n\n{}: {}".format(t["role"], t["content"].strip()) for t in turns)
        when = date
        if now:
            n = days_before(date, now)
            when += " (today)" if n == 0 else f" ({n} day{'s' if n != 1 else ''} before the current date)"
        history += "\n### Session {}:\nSession Date: {}\nSession Content:\n{}\n".format(i + 1, when, body)
    return history


def truncate_history(history: str, max_tokens: int) -> tuple[str, int]:
    """Upstream keeps the first max_tokens of the history in o200k_base
    tokens and drops the rest. Returns the history and its original length."""
    import tiktoken
    enc = tiktoken.get_encoding("o200k_base")
    tokens = enc.encode(history, allowed_special={"<|endoftext|>"})
    if len(tokens) <= max_tokens:
        return history, len(tokens)
    return enc.decode(tokens[:max_tokens]), len(tokens)


def reader_prompt(history: str, entry: dict[str, Any], cot: bool, facts: bool = False,
                  merge: bool = False) -> str:
    if merge:
        template = READER_PROMPT_MERGE_COT if cot else READER_PROMPT_MERGE
    elif facts:
        template = READER_PROMPT_FACTS_COT if cot else READER_PROMPT_FACTS
    else:
        template = READER_PROMPT_COT if cot else READER_PROMPT
    return template.format(history, entry["question_date"], entry["question"])


_clients: dict[str, Any] = {}

# Hidden reasoning tokens a reasoning model may spend before its answer.
REASONING_BUDGET = 8000


def reasoning_model(name: str) -> bool:
    return name.startswith(("gpt-5", "gpt-6", "o1", "o3", "o4"))


_usage_lock = __import__("threading").Lock()
USAGE: dict[str, dict[str, int]] = defaultdict(lambda: defaultdict(int))


def record_usage(model: str, usage: Any) -> None:
    """Tokens spent per model in this process, for cost estimates."""
    if usage is None:
        return
    details = getattr(usage, "completion_tokens_details", None)
    with _usage_lock:
        u = USAGE[model]
        u["calls"] += 1
        u["prompt"] += usage.prompt_tokens or 0
        u["completion"] += usage.completion_tokens or 0
        u["reasoning"] += getattr(details, "reasoning_tokens", 0) or 0


def complete(model: str, prompt: str, max_tokens: int, json_mode: bool = False) -> str:
    """One deterministic completion. `model` is "provider:name"; a bare name
    means OpenAI, which is what upstream uses for both reader and judge."""
    provider, _, name = model.partition(":") if ":" in model else ("openai", "", model)
    # Rate limits are retried for as long as the API asks, up to about an
    # hour of waiting; other errors get eight attempts.
    for attempt in range(200):
        try:
            if provider == "openai":
                if "openai" not in _clients:
                    from openai import OpenAI
                    _clients["openai"] = OpenAI()
                extra = {"response_format": {"type": "json_object"}} if json_mode else {}
                if reasoning_model(name):
                    # Reasoning models take max_completion_tokens, which also
                    # pays for their hidden reasoning, and only the default
                    # temperature. The answer budget is kept on top of that.
                    extra |= {"max_completion_tokens": max_tokens + REASONING_BUDGET}
                else:
                    extra |= {"temperature": 0, "max_tokens": max_tokens}
                r = _clients["openai"].chat.completions.create(
                    model=name, messages=[{"role": "user", "content": prompt}], n=1, **extra)
                record_usage(name, r.usage)
                return (r.choices[0].message.content or "").strip()
            if provider == "anthropic":
                if "anthropic" not in _clients:
                    import anthropic
                    _clients["anthropic"] = anthropic.Anthropic()
                params: dict[str, Any] = {"model": name, "max_tokens": max_tokens,
                                          "messages": [{"role": "user", "content": prompt}]}
                if name.startswith("claude-sonnet-5-5"):
                    # Sonnet 5.5 refuses sampling parameters and thinks by
                    # default, which would spend the answer budget; this
                    # turns thinking off, like the other readers.
                    params["thinking"] = {"type": "between_tools"}
                elif not name.startswith(("claude-opus-5", "claude-sonnet-5", "claude-fable", "claude-opus-4-7", "claude-opus-4-8")):
                    params["temperature"] = 0
                r = _clients["anthropic"].messages.create(**params)
                with _usage_lock:
                    u = USAGE[name]
                    u["calls"] += 1
                    u["prompt"] += r.usage.input_tokens or 0
                    u["completion"] += r.usage.output_tokens or 0
                return "".join(b.text for b in r.content if b.type == "text").strip()
            raise SystemExit(f"unknown provider {provider!r} in {model!r}")
        except SystemExit:
            raise
        except Exception as exc:  # rate limits and transient API errors
            message = str(exc)
            # A request larger than the per-minute token limit can never
            # succeed, and a spent quota will not refill by waiting.
            if "Request too large" in message or "insufficient_quota" in message:
                raise
            rate_limited = "rate_limit" in message or "429" in message
            if attempt >= 7 and not rate_limited:
                raise
            hint = re.search(r"try again in ([\d.]+)(ms|s)", message)
            if hint:
                wait = float(hint.group(1)) / (1000 if hint.group(2) == "ms" else 1)
            else:
                wait = min(60, 2 ** min(attempt, 6))
            wait += random.random() * 2
            print(f"  {model}: {type(exc).__name__}; retrying in {wait:.0f}s", flush=True)
            time.sleep(wait)
    raise RuntimeError(f"{model}: still rate limited after 200 attempts")


def read_jsonl(path: Path) -> list[dict[str, Any]]:
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def append_jsonl(path: Path, row: dict[str, Any]) -> None:
    with open(path, "a") as f:
        f.write(json.dumps(row) + "\n")


def run_dir(run_id: str) -> Path:
    d = HERE / "out" / run_id
    d.mkdir(parents=True, exist_ok=True)
    return d

