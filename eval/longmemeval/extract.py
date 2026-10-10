"""Turns one chat session into typed statements for remember(text=...).

The extractor is the part of a memory system that decides what to keep, so
it runs per session in date order, the way an agent would write memory as
conversations happen. It sees the subjects it has already used in this
haystack, so that a later session updates the same subject instead of
starting a new one, which is what lets supersession answer "what is it now".

Output is cached per (extractor model, prompt, registry, question), because
extraction is the expensive step and a rerun that only changes the reader or
k should not pay for it again.
"""
from __future__ import annotations

import hashlib
import json
import os
import re
from typing import Any

import lme

REGISTRY_PATH = lme.HERE / "registry.json"

PROMPT = """You maintain long-term memory for an assistant. Read one chat session between the user and the assistant, and record every fact that could matter in a later conversation.

Session date: {date}

Record facts as statements with these predicates only:
{predicates}

Rules:
- subject is the specific thing the fact is about, as a short lowercase name: the person, item, place, purchase, trip, activity, or topic, such as "united airlines frequent flyer status", "local park birdwatching", "sophia", "the nightingale by kristin hannah", "book from favorite author".
- Single-valued predicates (status, count, plan, location, relationship, date, duration, cost) keep only the newest value per subject, so a new value erases the old one. Never use them with subject "user" unless the fact describes the user as a whole (where the user lives, the user's job). "$30 book" and "$75 necklace" are two subjects, each with its own cost.
- Use subject "user" for event, preference, and detail statements about the user's life in general.
- Reuse a subject from this list when the fact is about the same thing, spelled exactly as listed: {subjects}
- value is a short, self-contained phrase. Resolve relative dates ("last week", "yesterday", "on 2/15") against the session date and write them as YYYY/MM/DD.
- evidence is an exact, verbatim excerpt of the session text that supports the statement, copied character for character, at most 200 characters.
- Keep specifics: names, numbers, places, dates, titles, counts, and amounts. Record facts the user states in passing, not only the topic of the conversation.
{events_rule}- Use assistant_said for specific content the assistant gave (a figure, a list item, a named recommendation). Skip generic advice.
- If nothing is worth keeping, return an empty list.

Return JSON only: {{"statements": [{{"subject": "...", "predicate": "...", "value": "...", "evidence": "..."}}]}}

Session:
{session}"""


# LME_EXTRACT_EVENTS=1 adds a rule that every dated happening is also an
# event, so "how many days between" questions have both dates. Without it the
# prompt is the one the published runs used (and their cache key).
EVENTS_RULE = ("- Whenever the user did, started, finished, bought, attended, joined, or changed something, also "
               "record an event with its resolved date, even when you record a status, preference, or detail "
               "about the same thing. A status keeps only the newest value; the event keeps when it happened.\n")
PROMPT = PROMPT.replace("{events_rule}", EVENTS_RULE if os.environ.get("LME_EXTRACT_EVENTS") else "")


# LME_OPEN_VOCAB=1 extracts against an open vocabulary, as Recall's open mode
# does: the model sees the predicates used so far in this haystack, reuses
# them, and coins new ones with a cardinality. Everything else in the prompt
# (subjects, values, evidence, dates) stays as tuned, so a comparison with the
# fixed registry measures the vocabulary alone.
OPEN = bool(os.environ.get("LME_OPEN_VOCAB"))

OPEN_PROMPT = (PROMPT
    .replace("""Record facts as statements with these predicates only:
{predicates}
""", """Predicates already in use in this memory:
{predicates}
""")
    .replace("""- Single-valued predicates (status, count, plan, location, relationship, date, duration, cost) keep only the newest value per subject, so a new value erases the old one. Never use them with subject "user" unless the fact describes the user as a whole (where the user lives, the user's job). "$30 book" and "$75 necklace" are two subjects, each with its own cost.
- Use subject "user" for event, preference, and detail statements about the user's life in general.""",
    """- predicate names the relation. Use a predicate from the list above whenever one can hold the fact, even when the session words it differently; most facts fit one. Coin a new predicate only when none can. A predicate is a short snake_case relation that many different facts could share, such as allergic_to or attends. Never put the value, topic, or place in its name (likes with value French wine, not interested_in_french_wine), and never name it after the subject.
- For a predicate you coin, choose cardinality multi unless the subject can hold only one value at a time and a new value makes the old one untrue (where someone lives now, a current job, a status, a price): single. Interests, experiences, events, possessions, people, and preferences add up. When unsure, choose multi. Give a one-line description. For a predicate from the list above, leave cardinality and description empty.
- A single-valued predicate keeps only the newest value per subject, so a new value erases the old one. Never use one with subject "user" unless the fact describes the user as a whole (where the user lives, the user's job). "$30 book" and "$75 necklace" are two subjects, each with its own values.
- Use subject "user" for facts about the user's life in general.""")
    .replace('''Return JSON only: {{"statements": [{{"subject": "...", "predicate": "...", "value": "...", "evidence": "..."}}]}}''',
             '''Return JSON only: {{"statements": [{{"subject": "...", "predicate": "...", "value": "...", "evidence": "...", "cardinality": "single, multi, or empty", "description": "..."}}]}}''')
    .replace(EVENTS_RULE, ""))
assert "with these predicates only" not in OPEN_PROMPT and "Coin a new predicate only" in OPEN_PROMPT and '"cardinality"' in OPEN_PROMPT


# The open vocabulary is seeded with Recall's personal starter, as an open
# server with no predicates file seeds it, so the extractor has generic
# relations to reuse from the first session.
SEEDS_PATH = lme.HERE.parent.parent / "registries" / "personal.json"


def seeds() -> dict[str, Any]:
    return {k: v for k, v in json.loads(SEEDS_PATH.read_text()).items() if k != "note"}


def active_prompt() -> str:
    return OPEN_PROMPT if OPEN else PROMPT


def predicate_name(name: str) -> str:
    """A predicate name the way Recall's open mode normalizes it."""
    return "_".join(re.findall(r"[a-z0-9]+", name.lower()))


def registry() -> dict[str, Any]:
    return json.loads(REGISTRY_PATH.read_text())


def predicate_table(reg: dict[str, Any]) -> str:
    if not reg:
        return "(none yet)"
    return "\n".join(f"- {name} ({spec['cardinality']}-valued): {spec['description']}" for name, spec in reg.items())


def cache_key(model: str) -> str:
    h = hashlib.sha256()
    for part in (model, active_prompt(), SEEDS_PATH.read_text() if OPEN else REGISTRY_PATH.read_text()):
        h.update(part.encode())
        h.update(b"\0")
    return h.hexdigest()[:12]


def parse(raw: str) -> list[dict[str, Any]]:
    text = raw.strip()
    if text.startswith("```"):
        text = text.strip("`")
        text = text[text.index("{"):] if "{" in text else text
    try:
        data = json.loads(text[text.index("{"): text.rindex("}") + 1])
    except ValueError:
        return []
    out = []
    for s in data.get("statements") or []:
        if isinstance(s, dict) and all(isinstance(s.get(k), str) and s[k].strip()
                                       for k in ("subject", "predicate", "value", "evidence")):
            st = {k: s[k].strip() for k in ("subject", "predicate", "value", "evidence")}
            if OPEN:
                st["predicate"] = predicate_name(st["predicate"])
                card = str(s.get("cardinality") or "").strip().lower()
                st["cardinality"] = card if card in ("single", "multi") else ""
                st["description"] = str(s.get("description") or "").strip()
            if st["predicate"]:
                out.append(st)
    return out


def extract(model: str, date: str, session_text: str, subjects: list[str], reg: dict[str, Any]) -> list[dict[str, Any]]:
    prompt = active_prompt().format(date=date, predicates=predicate_table(reg),
                           subjects=", ".join(f'"{s}"' for s in subjects[-300:]) or "(none yet)",
                           session=session_text)
    raw = lme.complete(model, prompt, 2000, json_mode=True)
    return parse(raw)


class Cache:
    """One JSONL file per question: a row per session with its statements."""

    def __init__(self, model: str, qid: str):
        self.path = lme.HERE / "out" / "extract-cache" / cache_key(model) / f"{qid}.jsonl"
        self.path.parent.mkdir(parents=True, exist_ok=True)
        meta = self.path.parent / "meta.json"
        if not meta.exists():
            meta.write_text(json.dumps({"model": model, "prompt": active_prompt(), "registry": seeds() if OPEN else registry()}, indent=2))
        self.rows = {r["session_id"]: r["statements"] for r in lme.read_jsonl(self.path)}

    def get(self, sid: str) -> list[dict[str, Any]] | None:
        return self.rows.get(sid)

    def put(self, sid: str, statements: list[dict[str, Any]]) -> None:
        self.rows[sid] = statements
        lme.append_jsonl(self.path, {"session_id": sid, "statements": statements})

