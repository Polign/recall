"""Score out/<run>/hyp.jsonl with the upstream LongMemEval judge.

    python judge.py --run full-4o

The prompts, the yes/no parse, temperature 0 and max_tokens 10 are copied
from xiaowu0162/LongMemEval src/evaluation/evaluate_qa.py. Keep the judge on
gpt-4o-2024-08-06 for any number that gets compared with published results.
Writes out/<run>/judge.jsonl and skips questions already judged.
"""
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed

import lme

_STANDARD = (
    "I will give you a question, a correct answer, and a response from a model. Please answer yes if the "
    "response contains the correct answer. Otherwise, answer no. If the response is equivalent to the correct "
    "answer or contains all the intermediate steps to get the correct answer, you should also answer yes. If "
    "the response only contains a subset of the information required by the answer, answer no. "
)
_TEMPORAL_EXTRA = (
    "In addition, do not penalize off-by-one errors for the number of days. If the question asks for the "
    "number of days/weeks/months, etc., and the model makes off-by-one errors (e.g., predicting 19 days when "
    "the answer is 18), the model's response is still correct. "
)
_TAIL = "\n\nQuestion: {}\n\nCorrect Answer: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only."

TEMPLATES = {
    "single-session-user": _STANDARD + _TAIL,
    "single-session-assistant": _STANDARD + _TAIL,
    "multi-session": _STANDARD + _TAIL,
    "temporal-reasoning": _STANDARD + _TEMPORAL_EXTRA + _TAIL,
    "knowledge-update": (
        "I will give you a question, a correct answer, and a response from a model. Please answer yes if the "
        "response contains the correct answer. Otherwise, answer no. If the response contains some previous "
        "information along with an updated answer, the response should be considered as correct as long as "
        "the updated answer is the required answer." + _TAIL
    ),
    "single-session-preference": (
        "I will give you a question, a rubric for desired personalized response, and a response from a model. "
        "Please answer yes if the response satisfies the desired response. Otherwise, answer no. The model does "
        "not need to reflect all the points in the rubric. The response is correct as long as it recalls and "
        "utilizes the user's personal information correctly.\n\nQuestion: {}\n\nRubric: {}\n\nModel Response: {}"
        "\n\nIs the model response correct? Answer yes or no only."
    ),
}
ABSTENTION = (
    "I will give you an unanswerable question, an explanation, and a response from a model. Please answer yes "
    "if the model correctly identifies the question as unanswerable. The model could say that the information "
    "is incomplete, or some other information is given but the asked information is not.\n\nQuestion: {}\n\n"
    "Explanation: {}\n\nModel Response: {}\n\nDoes the model correctly identify the question as unanswerable? "
    "Answer yes or no only."
)


def prompt(qtype: str, qid: str, question: str, answer: str, response: str) -> str:
    template = ABSTENTION if lme.is_abstention(qid) else TEMPLATES[qtype]
    return template.format(question, answer, response)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--run", required=True)
    ap.add_argument("--judge", default="gpt-4o-2024-08-06")
    ap.add_argument("--data", default="longmemeval_s_cleaned.json")
    ap.add_argument("--workers", type=int, default=8)
    args = ap.parse_args()

    refs = {e["question_id"]: e for e in lme.load(args.data)}
    out = lme.run_dir(args.run)
    judged_path = out / "judge.jsonl"
    done = {r["question_id"] for r in lme.read_jsonl(judged_path)}
    todo = [h for h in lme.read_jsonl(out / "hyp.jsonl") if h["question_id"] not in done]
    print(f"{args.run}: judging {len(todo)} ({len(done)} already judged) with {args.judge}", flush=True)

    def one(h: dict) -> dict:
        ref = refs[h["question_id"]]
        p = prompt(ref["question_type"], ref["question_id"], ref["question"], str(ref["answer"]), h["hypothesis"])
        verdict = lme.complete(args.judge, p, 10)
        return {"question_id": h["question_id"], "question_type": ref["question_type"],
                "abstention": lme.is_abstention(h["question_id"]), "judge": args.judge,
                "verdict": verdict, "label": "yes" in verdict.lower()}

    with ThreadPoolExecutor(args.workers) as pool:
        for f in as_completed([pool.submit(one, h) for h in todo]):
            lme.append_jsonl(judged_path, f.result())


if __name__ == "__main__":
    main()
