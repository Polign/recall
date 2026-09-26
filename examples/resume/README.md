# Resuming an agent after a crash

This example resumes an agent from its own records instead of a snapshot. It
needs no model: the "agent" works through a fixed five-step task, and each
step records a turn and updates the working state.

Start a local Polign server with the lease API, then run:

```sh
go run ./examples/resume -crash-at 3   # does steps 1 and 2, then exits without releasing
go run ./examples/resume               # waits out the lease, then continues at step 3
```

The second run prints the briefing it would hand a model: the goal, the
remaining plan, what is done, and the last turns verbatim. It then carries on
from step 3, because step 2 is the last one its note records.

Run it once more and it resumes a finished task: the briefing shows all five
steps done and there is nothing left to do. Use `-agent` with a new name to
start over.

`-lease-ttl` sets how long a crashed run blocks the next one (10 seconds
here, 60 by default in the library). A clean exit releases the lease, so the
next run starts at once.
