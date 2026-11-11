# Tool Limit Wrap-Up

What the agent does when a turn exhausts `max_tool_iterations`.

## Why

Historically, a turn that burned its tool budget ended with a hardcoded string:

> I've reached `max_tool_iterations` without a final response. Increase
> `max_tool_iterations` in config.json if this task needs more tool steps.

That message discarded everything the turn had actually accomplished — even though
`turnState` was already tracking every tool call, its success, and its error summary for
the `turn.end` event. A turn that ran 50 careful iterations and one that hit rate limits
on the first call produced identical output, and there was nothing to act on.

## What happens now

When the tool budget is spent, the turn is granted **one extra, tool-less LLM call** to
summarise what it did, what remains, and the best next step. The result is a message
that always contains a deterministic baseline, enriched by the model when that call
succeeds.

```
⏳ Tool budget reached — 50 of 50 iterations used.

I refactored the session allocator and the unit tests pass, but I did not get to
updating the routing docs. Next step: run the routing tests.

Tools used
- `read_file` ×12
- `edit_file` ×8 (2 failed)
- `exec` ×6

Errors
- exit status 1

Skills used
- `dev-cycle`

The turn ended here. Raise `max_tool_iterations` (currently 50) in config.json if this
task needs more tool steps.
```

## Design decisions

| Decision | Choice | Rationale |
| --- | --- | --- |
| Budget for the wrap-up call | **Extra call** (`N` tool iterations + 1 wrap-up) | The configured budget is honoured exactly rather than silently reduced. The loop's existing graceful-interrupt clause already permits it, so no loop-condition change was needed. |
| How the summary is produced | **Hybrid** — deterministic baseline, enriched by one LLM call | The feature fires exactly when turns run long and providers may be flaky. Baseline-first means a failed enrichment still yields a useful message instead of reproducing the dead end. |
| Content | Budget, tool usage with ✅/❌ counts, one-line errors, skills used, and a next-step prompt | Diagnostic enough to explain *why* the budget was spent. |
| Turn fate | **End the turn** | Session history is saved, so a re-prompt resumes naturally. Auto-continue would undermine the cap. |
| Turn status | Unchanged (`completed`) | Avoids rippling through every `TurnEndStatus` consumer. |
| Config surface | None — always on | Strictly better than the static string; no config debt. |
| Sub-turns | Baseline only, no extra provider call | Sub-turns have no human reader; the parent consumes `ForLLM`. |

## Implementation

| Piece | Location |
| --- | --- |
| Wrap-up trigger | `pkg/agent/tool_limit.go` — `maybeStartToolLimitWrapUp` |
| Baseline renderer | `pkg/agent/tool_limit.go` — `buildToolLimitSummary` |
| Hint for the model | `pkg/agent/tool_limit.go` — `toolLimitWrapUpHint` |
| Loop integration | `pkg/agent/turn_coord.go` — fires after tool execution, renders before finalize |
| Sticky flag | `pkg/agent/turn_state.go` — `toolLimitWrapUp` |

The wrap-up reuses the existing graceful interrupt machinery: requesting one sets a
one-shot flag, and the next iteration runs with tools stripped. `pipeline_llm.go` then
discards any tool calls the model emits anyway, taking its text as the final answer — so
the model cannot extend its own budget.

## Failure handling

The baseline is rendered from `toolExecutions` regardless of what the model returns:

- **Wrap-up call errors** → the error is logged and swallowed; the baseline is still
  shown. A long turn is precisely when the provider is most likely to fail, so surfacing
  an error would reproduce the original problem.
- **Wrap-up returns empty** → same path; the baseline is shown.
- **Model emits tool calls anyway** → discarded (see above).

## What is deliberately out of scope

- **Auto-continue with a fresh budget** — would undermine the cap; needs its own budget
  rules as a separate proposal.
- **A `TurnEndStatusToolLimit` status** — considered, deferred. Turns still report
  `completed`. If tuning the limit becomes a priority, this is the natural follow-up: it
  would make "how often do I hit the limit?" queryable from `turn.end` events.
- **Config to disable it** — there is no compelling behaviour to preserve.

## Tuning the limit

The message reports the current value so it can be negotiated, and the tool table
usually reveals the real cause. A turn showing `exec ×30` is a runaway loop, not a limit
set too low; raising `max_tool_iterations` will only make it longer.
