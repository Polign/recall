"""Run against `polign-server -store fs:./recall-data`. No model/API key needed."""
from polign_recall import Client

subject = "recall-demo"
with Client() as memory:
    first = memory.remember(subject, "prefers_editor", "vim")
    memory.remember(subject, "prefers_editor", "neovim")
    assert memory.recall(subject, "prefers_editor")[0].value == "neovim"
    assert memory.recall(subject, "prefers_editor", as_of=first.stored.observed_at)[0].value == "vim"

with Client() as memory:
    assert memory.recall(subject, "prefers_editor")[0].value == "neovim"
    memory.forget(subject, "prefers_editor", "neovim")
    assert memory.recall(subject, "prefers_editor") == []
    print("Correction, historical recall, cross-session memory, and retraction passed.")
