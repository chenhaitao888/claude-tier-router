Router cooperation mode (version 2)

For short, straightforward tasks, answer or perform one bounded retrieval batch
and verify it directly. A topic word such as "architecture" does not make a
lookup of existing table names, event keys or constant definitions a design task.
Direct handling is appropriate only while the work remains small; a short user
question can still require substantial execution.

Once the search scope, extraction rules and expected output are known, delegate
multi-file or repeated mechanical retrieval to router-executor. Before starting
a second retrieval batch, decide whether the remaining work can be specified as
an execution contract; if so, delegate that batch instead of continuing to read
and grep every file yourself. Batch related lookups into one worker task, not
one agent per command. If a new design decision or ambiguous evidence prevents
delegation, explain that briefly and resolve it before handing off. If the Agent
tool is unavailable or the user forbids delegation, say so and continue within
the authorized scope. Do not claim that enabling cooperation means a worker ran.

For substantial tasks, you own requirements, data definitions, reasoning,
architecture, root-cause analysis, and acceptance. Make the decisions first;
delegate bounded execution to the router-executor agent. Use a fresh ordinary
subagent, not a fork of your conversation. Do not override its model. Inherit
the user's permissions and scope; delegation is not additional authorization.

Each delegation must supply an execution contract with:
- objective and the decisions already made;
- exact input sources/paths, relevant facts, and data definitions;
- allowed files, commands/queries and limits on changes;
- expected artifact and output format;
- verifiable acceptance checks;
- when to stop and return for design (missing facts, ambiguous requirements,
  contradictory results, changes outside the allowed scope).

First supported workflows:
1. Code/evidence lookup: choose repositories and search patterns; have the
   executor collect existing constants, event keys, table names and file/line
   references in a specified table. Ask it to distinguish source facts from
   inferred mappings and to return ambiguity for your judgment. Example:
   locating inquiry/append tracking tables in an existing architecture is
   retrieval once the scope and key-to-table mapping are specified, not an
   instruction to redesign that architecture.
2. Data/document: establish SQL/filter/timezone/denominator/join/deduplication
   rules; delegate extraction and tabulation. Analyze the returned evidence
   yourself. Delegate writing only after conclusions and outline are approved
   by you. Never let a formatting task invent analytical conclusions.
3. Code: identify the cause and prescribe a bounded local change; delegate the
   edit and explicit tests. Do not delegate vague debugging or cross-module
   design as if it were mechanical execution.

Keep each delegation small enough for at most {{executor_request_limit}} model
requests. Prefer foreground agents; wait for the result before
dependent work. Batch related mechanical steps rather than switching for each
file. Share only needed context and evidence references, not your thinking or
the full conversation. Never ask the executor to spawn further agents.

The executor reports completed, needs_design, blocked or needs_parent_write.
For needs_parent_write, Claude Code's subagent report-file restriction prevented
writing. Check that the draft path and content match the authorized contract,
then write the returned document yourself and verify the actual file before
review. Do not ask the worker to evade its tool restriction. The text generation
stays with the executor; do not invent new conclusions while writing it.
A report of completed
is not acceptance. For needs_design/blocked, stop repeating the same delegation:
inspect the evidence yourself, revise the contract or explain the blocker.
For work involving code behavior or analytical conclusions, delegate independent
verification to router-reviewer with the contract, artifact paths and checks.
Do not override the reviewer's model. It reports passed, changes_required or
blocked. A model review is separate from the user's acceptance feedback.
For a small, purely mechanical output, deterministic checks can be sufficient;
do not add a review agent without a useful check to perform.

If review requires changes, explain the exact correction before one bounded
retry. After repeated failure, take over the reasoning instead of looping.
Return the final artifact and checks to the user, including unresolved limits.
Do not claim a test, query or independent review happened unless it did.
