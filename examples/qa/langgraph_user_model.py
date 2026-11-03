"""A per-user model that nodes and tools update and read at runtime, where a
newer fact replaces an older one and the old value stays in the history."""
from typing import Annotated
from langchain_core.tools import InjectedToolArg, tool
from langgraph.graph import START, MessagesState, StateGraph
from polign_recall import Client                     # pip install polign-recall

memory = Client(local_dir="./recall-data", typed=True,
                env={"RECALL_PREDICATES": "registry.json"})   # the fields of your user model


@tool
def update_user(field: str, value: str, user_id: Annotated[str, InjectedToolArg]) -> str:
    """Record something the user said about themselves."""
    changed = memory.remember(user_id, field, value)
    return f"{field} = {value}" + (f" (was {changed.superseded[0].value})" if changed.superseded else "")


def load_user_model(user_id: str) -> str:
    """Current beliefs about the user, for the system prompt: one line per field."""
    return "\n".join(f"{b.predicate}: {b.value}" for b in memory.recall(user_id))


# A node reads the model before calling the LLM; a tool writes to it.
def plan(state: MessagesState, config):
    user_id = config["configurable"]["user_id"]
    profile = load_user_model(user_id)
    return {"messages": [("system", f"What you know about this user:\n{profile}")]}

builder = StateGraph(MessagesState)
builder.add_node("plan", plan)
builder.add_edge(START, "plan")
graph = builder.compile()

if __name__ == "__main__":
    update_user.invoke({"field": "plan_tier", "value": "team", "user_id": "u1"})
    print(update_user.invoke({"field": "plan_tier", "value": "enterprise", "user_id": "u1"}))
    out = graph.invoke({"messages": []}, {"configurable": {"user_id": "u1"}})
    print(out["messages"][-1].content)
    print([ (s.change, s.began) for s in memory.timeline("u1", "plan_tier").changes ])
