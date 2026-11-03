# Snippets from answers

Every snippet posted in an answer on a forum or an issue lives here, so the
answer can link to a file that is kept current when an API changes. Each one
ran on the date in its header against the released packages it names.

| File | Posted at | Needs |
|---|---|---|
| temporal_session.py | temporalio/sdk-python#1890 | `pip install recall-openai-agents`, `OPENAI_API_KEY` |
| langgraph_user_model.py | forum.langchain.com/t/3884 | `pip install polign-recall langgraph langchain-core`, `registry.json` beside it |

Run one with `python <file>` from this directory. Both keep their database in
`./recall-data`.
