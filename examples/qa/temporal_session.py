"""Keep the conversation outside Temporal's workflow history: one workflow per
user turn, with the history in a session the Agents SDK reads and writes."""
import asyncio

from agents import Agent, Runner
from recall_openai_agents import RecallSession   # pip install recall-openai-agents


async def one_turn(user_id: str, text: str) -> str:
    # In Temporal, this is the body of an activity (or a short workflow per turn):
    # the session id is the only thing the workflow input needs to carry.
    session = RecallSession(f"chat-{user_id}", local_dir="./recall-data")  # or backend="pgvector", backend_url=...
    agent = Agent(name="assistant", instructions="Reply briefly.")
    result = await Runner.run(agent, text, session=session)              # reads the history, appends this turn
    return result.final_output


async def main():
    print(await one_turn("u1", "Hi, I am Sam, and we deploy to GKE."))
    print(await one_turn("u1", "Where do we deploy?"))                   # a new process would answer the same
    history = await RecallSession("chat-u1", local_dir="./recall-data").get_items()
    print(len(history), "items in the session, none of them in workflow history")

asyncio.run(main())
