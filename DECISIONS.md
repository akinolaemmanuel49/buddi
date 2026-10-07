# Decisions

A record of the decisions made while building Buddi, and why. Each entry states
the situation, the choice, the reasoning, and what it costs us. Decisions are
numbered so they can be cited from code comments and pull requests.

Newest entries are appended at the end of their section. When a decision is
reversed, the original entry is marked **Superseded** rather than deleted, and
the replacement is added as a new entry.

Related documents: `README.md` (how to run it), `MVP_TODO.md` (what is left).

---

## 1. Product shape

### 1.1 Backend first, API containerization last

**Context.** The MVP definition of done spans local LLM inference, RAG, agent
orchestration, tool use, and MCP. Frontend and packaging both consume an API
whose shape is still moving.

**Decision.** Build and verify the backend end to end on the host first.
Containerize the API in the final stage, after the core workflow is proven.

**Why.** Every architectural decision that matters here — whether planning is
synchronous, whether a run survives a disconnect, what the tool seam looks like
— is cheaper to change before anything depends on it. Packaging an API that is
still reshaping its endpoints produces throwaway work. Running on the host also
makes the CPU-bound model timings measurable without container overhead.

**Cost.** Not deployable until late. Accepted.

### 1.2 Compose profiles for optional services

**Context.** `docker compose up -d db` would otherwise pull a multi-gigabyte
Ollama image for someone working only on the API.

**Decision.** Optional services live behind profiles: `llm` for Ollama, `obs`
for the OTel collector and Jaeger.

**Why.** The default path should be fast and cheap. A developer touching
handlers should not pay for model weights they never load.

**Cost.** One extra flag to remember: `docker compose --profile llm up -d ollama`.

---

## 2. Model and inference

### 2.1 `qwen3:0.6b` as the only general model

**Context.** The target machine has no GPU. Ollama reported roughly 7.4 GiB of
memory in use across the service, and CPU inference runs at roughly 20–50
tokens/second with thinking disabled, about 2.5 tokens/second while thinking.

**Decision.** Use `qwen3:0.6b` for planning. Do not add a larger tier.

**Why.** Memory, not ambition, sets the ceiling. A larger model would not fit
alongside the embedding model in shared RAM. Choosing the small model
deliberately, rather than as a stopgap, keeps the memory budget honest and makes
its quality limits something we design around instead of discover later.

**Revisit when.** A machine with a GPU, or a demonstrated need the 0.6b model
cannot meet.

### 2.2 One model in use, not a model per stage

**Context.** Separate planner and chat models would be more flexible.

**Decision.** `PlannerModel` falls back to `Model` when unset. There is
deliberately only one general-purpose model in the config.

**Why.** Each resident model competes for the same scarce RAM, and latency
already runs 10–20 seconds per plan. A second general model would roughly halve
the number of concurrent requests the box can serve.

**Cost.** No cheap model for a hot path. Revisit if a second stage appears.

### 2.3 `think:"low"` rather than disabling thinking

**Context.** Ollama accepts a boolean or a graded level in the same `think`
field. Measured over three planning attempts:

| Approach | Valid output | Avg latency | Reasoning tokens |
| --- | --- | --- | --- |
| `format` + default thinking | 3/3 | 9.1s | ~359 |
| `format` + `think:"low"` | 3/3 | 5.0s | ~216 |
| Schema described in prose | 3/3 | 9.4s | included above |

**Decision.** Planner requests `think:"low"`.

**Why.** `think:false` is faster still but **cannot be combined with
`format`**, so schema-constrained output has to budget for reasoning rather than
suppressing it. Graded `low` was also not merely cheaper — it produced a better
title than the default, so quality and latency pointed the same way.

**Cost.** A small reasoning tax on every planning call.

### 2.4 Schema-constrained output, not a schema described in prose

**Context.** Small models are unreliable at holding a JSON shape from
instructions alone.

**Decision.** Send a JSON Schema as a nested object in Ollama's `format` field.

**Why.** Measured both reliable and faster than describing the schema in prose
(see table in 2.3). The constraint is enforced by the decoder rather than
requested politely.

**Cost.** The schema is part of the contract, so a field change means the schema
change with it. Also, `format` must be an object in the request body — a string
containing JSON is silently not the same thing.

### 2.5 `num_predict` of 1024

**Context.** The planner budget is spent on reasoning before any plan is emitted.

**Decision.** 1024 tokens.

**Why.** At 512 tokens a planning call twice exhausted the entire budget on
reasoning and returned **nothing at all** — a silent empty result, not a
truncated one. Budget generously rather than optimising the ceiling.

### 2.6 Flash attention left off

**Context.** It is a common performance default.

**Decision.** Not enabled.

**Why.** It requires a supported accelerator. On CPU it would be a setting that
does nothing while implying a speed-up that never arrives — the worst kind of
config.

### 2.7 KV cache quantised, context capped

**Decision.** `OLLAMA_KV_CACHE_TYPE=q8_0` and `OLLAMA_CONTEXT_LENGTH=8192`,
with `OLLAMA_MAX_LOADED_MODELS=1` and `OLLAMA_NUM_PARALLEL=1`.

**Why.** Context length is the dominant latency cost on CPU *and* the largest
memory lever, since the KV cache grows with it. `q8_0` halves KV memory and does
engage on CPU. Extra resident models or parallel requests only thrash a machine
that already fits its workload in memory.

### 2.8 Model cannot do date arithmetic — degrade, never mislead

**Context.** Asked to plan something "before Friday", the model returned the
current date, both before and after the prompt was given the current time. It
cannot add days.

**Decision.** The planner drops a due date that has already passed or that
merely echoes the request instant, and the prompt instructs the model to use the
current time *only* to derive `due_at`.

**Why.** The failure mode should be "no deadline" rather than a confidently
wrong one. A wrong due date is worse than a missing one, because a user
re-trusting it acts on bad information.

**Cost.** Deadlines that need real arithmetic ("Friday", "next Tuesday") are
often dropped. Long term the fix is resolving relative dates in code
(`MVP_TODO`), not in the model.

### 2.9 Prompt hygiene against instruction leakage

**Context.** The prompt must state the current time, and `qwen3:0.6b` sometimes
pasted that timestamp straight into the title — a live run produced `Buy
semi-skimmed milk at 2026-10-04T08:27:56Z`, which then became a real stored task
title.

**Decision.** Instruct the model explicitly never to mention the current date or
time in the title or steps, in both the base and repair prompts. The live smoke
test asserts titles contain no timestamp.

**Why.** The prompt has to contain information the model tends to copy. Stating
the rule is cheap; a live test makes regression visible instead of leaving it to
be discovered in a task list.

**Known limitation.** Steps still occasionally carry a timestamp. The smoke test
logs it rather than failing, because a plan with a dated step is still usable.
Titles are enforced because a title is the thing users read first.

---

## 3. Planning

### 3.1 Validate, repair, retry — never fail the request

**Decision.** A plan is validated against the domain. Failures are split into
**fatal** (retry) and **repairs** (accept after adjusting), and after
`BUDDI_PLAN_MAX_RETRIES` a deterministic fallback plan is returned.

**Why.** Small models produce imperfect plans routinely. Treating every defect as
a hard failure would make the feature unusable on the model we can afford. The
split matters: a bad priority is repairable, an unparseable response is worth
one more attempt.

**Cost.** A fallback plan can be generic. It is recorded on the run so the
outcome is never mistaken for a good plan.

### 3.1a A timeout is *not* a fallback case

**Context.** Verified live by setting the planning budget to 2s. The endpoint
answered `201` with `awaiting_approval`, no error, and a plan identical to the
fallback shape. The cause was in 3.1's error branch: any generation error broke
out of the retry loop and substituted the fallback, including a deadline.

**Decision.** `context.DeadlineExceeded` and `context.Canceled` propagate as
errors. They do not produce a fallback plan.

**Why.** A timeout is not the model answering badly — it is the model never
answering. The fallback is built from the user's own goal, so it renders as a
tidy title and reads exactly like a considered plan. A user approving it believes
the model reasoned about the request when nothing was ever generated. For an
agent whose entire premise is "the model proposes, you review", silently
fabricating the proposal is the worst available failure. It also made the run's
`failed` status and the failure-recording machinery in 4.1 unreachable for the
one failure users are most likely to hit.

**Consequence.** A slow machine now gets an honest `504` and a `failed` run with
the cause recorded, instead of a plausible-looking plan.

### 3.1b Fallback plans are labelled, not passed off

**Decision.** `agent_runs.plan_fallback` (migration 2) records that a plan was
the deterministic fallback, and the API always serialises it, including when
false.

**Why.** 3.1a's fix stops timeouts reaching the fallback, but a schema-invalid
plan still legitimately can. The flag is what lets a client say "this was
salvaged" rather than presenting it as the model's considered answer. It is
always present in the JSON so an absent field cannot be mistaken for `false`.

**Related bug, same shape.** `agent_runs.trace_id` was documented as populated
while the helper was never called, and then — after being wired — the field was
still missing from responses, because the API serialises its own `runResponse`
struct rather than the domain model. Both were invisible to service-layer tests.
Covered now by a service test and a handler-level test; see 8.3.

### 3.2 Priorities come from the domain, not the prompt

**Decision.** The JSON Schema enumerates priorities by reading
`domain.AllTaskPriorities` at request time.

**Why.** The model cannot invent a priority that the domain does not accept, and
adding a priority does not require editing a prompt string in two places.

### 3.3 Inject a clock rather than letting the model guess

**Decision.** The prompt states the current time from an injectable clock.

**Why.** Deterministic tests need a fixed "now", and a model guessing the date
produces plans that cannot be reasoned about. Trade-off documented in 2.8 and
2.9.

---

## 4. Agent orchestration

### 4.1 The run is created before planning starts

**Decision.** `POST /agent/runs` inserts the run first, then plans.

**Why.** Finalisation runs on a detached context (4.3), so the request can fail
or the client can vanish and still leave a record. Creating the run up front is
what makes that guarantee meaningful.

**Consequence.** A run exists even if planning never succeeds, in `failed`
status with the error attached. That is the intended behaviour: an inspectable
failure beats a silent no-op.

### 4.2 Planning is synchronous — no worker, no queue

**Context.** Measured 10–20s per plan on CPU.

**Decision.** `POST /agent/runs` blocks for the model. No background worker,
no job table, no polling.

**Why.** A worker would add a queue, retry policy, and a second state machine to
a project with no throughput requirement yet. Blocking keeps the whole lifecycle
visible in one function and one stack trace.

**Consequences**, all documented in the README:
- `BUDDI_AGENT_PLAN_TIMEOUT` (default 45s) bounds the model call.
- The endpoint is **not idempotent**. A client that times out and retries creates
  a second run, because the first may still be planning.
- Concurrency is bounded by CPU, not by design.

**Revisit when.** Several users, or any plan that reliably exceeds the request
timeout.

### 4.3 Plan timeout must be shorter than the request timeout

**Decision.** Config validation rejects a plan timeout at or above
`REQUEST_TIMEOUT` (60s default).

**Why.** Planning is answered synchronously. If the model outlives the server's
write timeout, the client receives a truncated response and the run's outcome is
never recorded — the exact silent failure 4.1 exists to prevent. Catching the
misconfiguration at startup is far better than debugging it in production.

### 4.4 A disconnect does not cancel the run

**Decision.** Finalisation uses `context.WithoutCancel`. If the client hangs up
mid-plan, the run still reaches `awaiting_approval`, and the caller can pick it
up from `GET /agent/runs/:id`.

**Why.** The alternative is that cancelling on disconnect makes the work
silently never happen. The user asked for the plan; losing it because a
connection dropped is worse than an extra orphaned record.

**Cost.** Runs can exist that no one is waiting on. `GET /agent/runs?status=`
exists to find them.

### 4.5 Rejecting an approval cancels the whole run

**Decision.** `POST /agent/approvals/:id/reject` moves the run to `cancelled`
and applies nothing. Approving afterwards returns `409`.

**Why.** Reviewing a proposal as a whole is the clearer contract: the user saw
one plan and declined it. Skipping one step and silently executing the rest
would mean applying a plan the user never agreed to.

**Revisit when.** Multi-tool runs are common enough that per-step skipping earns
its ambiguity. Not yet: one approval currently maps to one `tasks.create`.

### 4.6 One task per plan, steps carried in the description

**Decision.** A plan becomes a single task. The plan's steps are written into
the task description as a numbered list.

**Why.** The planner's schema produces one item of work with supporting steps.
Creating a task per step would turn "email the quarterly report" into three
near-duplicate tasks, which is worse than one task with a checklist in it.

**Revisit when.** The model demonstrably benefits from per-step tasks.

### 4.7 Approving twice is a conflict

**Decision.** A second approve or reject returns `409`.

**Why.** Approval is a state transition guarded by the current status. Making it
idempotent would hide a real client bug — a double-submitted button — behind a
success response.

### 4.8 The `Tool` interface is the MCP seam

**Decision.** Execution goes through a `Tool` interface. Today's implementation
is in-process; a later one will be an MCP client.

**Why.** The orchestration around execution — approvals, timeouts, run status,
results — should not change when the transport does. Getting this boundary right
now means MCP is a substitution rather than a rewrite.

---

## 5. Security

### 5.1 JWT access tokens with rotating refresh tokens

**Decision.** Short-lived access token (15 min), long-lived refresh (168h), with
rotation tracked by token family.

**Why.** Refresh tokens are long-lived credentials, so a stolen one must stop
working. Rotation plus family tracking lets reuse of an already-rotated token be
detected and the family revoked.

### 5.2 bcrypt cost 12

**Decision.** Default cost 12.

**Why.** Strong enough to make offline cracking expensive, and fast enough that
login stays interactive on this CPU.

### 5.3 Tenant scoping in every query, and 404 not 403

**Decision.** Every query filters by user id. A record belonging to another user
returns `404`, never `403`.

**Why.** A `403` confirms the resource exists. For a personal assistant holding
notes and tasks, existence is itself private, so a cross-tenant request must be
indistinguishable from a request for something that does not exist.

### 5.4 Strict JSON decoding

**Decision.** Unknown fields are rejected with `400 bad_request`.

**Why.** A silently ignored misspelled field is a bug that surfaces much later
as missing data. Rejecting is louder and cheaper to debug.

### 5.5 `citext` for email uniqueness

**Decision.** Emails are `citext`.

**Why.** Case-insensitive comparison in the database, so uniqueness cannot be
bypassed by capitalisation.

---

## 6. Persistence

### 6.1 Migrations embedded in the binary

**Decision.** Migrations are embedded SQL applied at startup. Currently two:
`000001` (initial schema) and `000002` (adds `plan_fallback`).

**Why.** No separate migration step and no ordering to remember for local
development. 000001 was left untouched once it had run against a real database —
amending an applied migration silently leaves existing databases on the old
shape, so a new migration is the honest way to change a schema that has been
executed somewhere.

**Revisit when.** Multiple instances start concurrently against a shared
database, where advisory-lock coordination becomes necessary.

### 6.2 HNSW with cosine distance

**Decision.** `note_chunks.embedding` is `vector(768)` with an HNSW index using
`vector_cosine_ops`, `m=16`, `ef_construction=64`.

**Why.** Cosine similarity is the right measure for normalised embeddings, and
HNSW gives good recall at low latency without the training step IVFFlat needs.
`m=16` and `ef_construction=64` are conservative defaults that build faster and
use less memory than larger values — appropriate for a dataset that starts empty.

### 6.3 Embeddings are nullable, dimension is enforced

**Decision.** `embedding vector(768)` is nullable, with
`CHECK (vector_dims(embedding) = 768)`.

**Why.** Retrieval is not implemented yet, and notes must be creatable before
anything is chunked. The dimension check is still enforced whenever an embedding
is present, so a wrong-dimension embedding cannot be stored silently.

**Cost.** The vector index contains nulls until the retrieval workflow lands.

**Update.** Retrieval has since landed (section 9), so the nulls are transient
rather than permanent: every note is embedded when it is written, and anything
that is missed stays queued until it is not.

### 6.4 `user_id` denormalised onto `note_chunks`

**Decision.** `note_chunks` carries its own `user_id`, indexed, rather than
relying on a join through `notes`.

**Why.** Keeps the HNSW scan on a single table and allows the tenant filter to be
applied without a join on every search.

---

## 7. Observability

### 7.1 Tracing off by default, with a no-op pipeline

**Decision.** No exporter is installed unless `OTEL_EXPORTER_OTLP_ENDPOINT` is
set; otherwise a no-op pipeline runs.

**Why.** Zero overhead and zero setup for normal development, while local
end-to-end runs can enable real tracing with `--profile obs`.

### 7.2 Trace id persisted on the run

**Decision.** `agent_runs.trace_id` stores the request's trace id.

**Why.** A run that took 15 seconds across a model call and a tool call needs to
be findable in the tracing backend starting from the database.

**Corrected during implementation.** The helper existed from the start but was
never called, so the column was always empty while the README claimed otherwise.
Live end-to-end verification is what surfaced it.

### 7.3 Request id on every error

**Decision.** Errors carry a `request_id` matching the log line.

**Why.** A user-reported failure needs to be traceable to a specific log entry
without correlating by timestamp.

---

## 8. Testing

### 8.1 Live model tests are gated behind environment variables

**Decision.** Smoke tests against real Ollama only run when explicitly enabled
(e.g. `BUDDI_SMOKE_PLANNER=1`).

**Why.** They need a running model, take tens of seconds, and would make the
suite unusable offline or in CI without a model.

**Cost.** These tests do not run by default, so nothing catches a model-behaviour
regression automatically. Mitigation: assertions inside them are real (schema
shape, no timestamp leakage in titles, 3/3 usable), not smoke-for-smoke's-sake.

### 8.2 Planner logic tested against a fake generator

**Decision.** Retry, repair, fallback, and validation paths are covered with a
deterministic fake generator.

**Why.** Those branches are the ones that matter and the ones that are painful to
trigger on demand from a real model. A fake makes every branch reachable.

### 8.3 Test the layer the request actually goes through

**Context.** Two defects survived because they were tested one layer above where
they lived. A planning timeout answered `500` because the agent handlers call
`writeServiceError`, while the test I wrote exercised `WriteError` — a different
function with its own switch. `plan_fallback` was persisted and returned by the
service but never appeared in responses, because the API builds its own
`runResponse` instead of serialising the domain model.

**Decision.** Where a value crosses a layer boundary, assert on it at the
boundary. The API has a `fakeAgentService` and a handler-level test for the run
response; error mapping is tested on `writeServiceError`, not only `WriteError`.

**Why.** A green service-layer suite says the orchestration is correct. It says
nothing about whether the client can see the result. Both of these bugs produced
a correct database and a correct service, and a wrong answer to the user.

### 8.4 Prove failure paths live, not only in unit tests

**Context.** 4.2 and 4.3 shipped on the strength of config validation and unit
tests. A 2s budget showed the behaviour was wrong in a way no test predicted: not
a hang, and not a failure, but a *plausible wrong answer*.

**Decision.** Fault-inject against the real model for the paths that decide what
the user sees. Timeouts are proven by setting `BUDDI_AGENT_PLAN_TIMEOUT=2s` and
observing `504`, a `failed` run with the cause, and nothing applied.

**Why.** A small model's failure modes are not the ones a fake generates. The
unit tests encoded what I expected; the live run showed what actually happens.
Only the live check surfaced the fallback defect, and it is the defect most likely
to be approved by a trusting user.

---

## 9. Retrieval and indexing

### 9.1 Search state lives on the note row

**Decision.** `notes` carries `search_state`, `index_attempts`,
`last_index_error`, `next_index_at` and `indexed_at`, and every write of note
text sets `search_state = 'pending'` in the same statement that stores the text.

**Why.** "The note was saved" and "the note is queued to be searchable" have to
be the same commit. A separate queue table or an after-write call leaves a
window where the text is stored and nothing will ever index it, and that window
is invisible until a user searches for something they just wrote. Carrying the
flag on the row closes it.

**Cost.** Five columns on the hottest table in the database, and every future
write path has to remember the flag. Mitigated by the service being the only
thing that writes notes.

### 9.2 'indexed' means every chunk has an embedding, not some of them

**Decision.** A note is `indexed` only when it has at least one chunk and no
chunk is missing an embedding. A partial index is never reported as done.

**Why.** A half-embedded note that claims to be searchable returns answers
grounded in the first few hundred words while the rest of the text is invisible,
and nothing re-queues it because the state says the work is finished. The same
predicate is used by the backfill (9.8), so a database that predates the state
column is labelled by the same rule as one that is being written by the service.

### 9.3 Index inline on save, and let a worker finish what inline could not

**Decision.** A note write calls the indexer inline, best-effort. A background
worker in the same process claims and indexes whatever is still queued, on a
lease, with retries.

**Why.** Inline indexing makes a note searchable by the time the response
returns, which is the behaviour a person expects from "saved". But inline is
best-effort: it holds a request open for several embedding calls, and it fails
whenever the model runtime is down — which is most of the time a user is not
writing notes. Without a worker, that failure is permanent: the note stays
unsearchable until the user edits it again. Running both is not redundant,
because the two cover opposite failures.

**Cost.** Two paths reach the same bookkeeping. It is shared (`indexBookkeeping`)
so backoff, the attempt cap and error reporting cannot drift between them.

### 9.4 One statement claims the batch, with the lease stored on the note

**Decision.** `ClaimDueNotesForIndexing` is a single statement: a CTE selects
due notes `FOR UPDATE SKIP LOCKED`, then updates them to `'indexing'` and pushes
`next_index_at` to now + lease, returning the rows.

**Why.** A select followed by an update has a gap between the two statements, and
that gap is exactly where two workers end up embedding the same note. `SKIP
LOCKED` also means the queue drains in parallel instead of every worker queueing
behind the same first row. The lease lives in `next_index_at` rather than in a new
column because the queue is already ordered by that column and adding a second one
means a second thing to keep consistent.

**Cost.** The query is the most intricate statement in the codebase. It is
covered by live PostgreSQL tests rather than only by a mock, because a mock
cannot tell a correct claim from a plausible one.

### 9.5 A dead worker's note is reclaimed by the same query

**Decision.** `'indexing'` is claimable once its lease expires. No sweeper, no
separate "stuck" query.

**Why.** The failure mode is a process that dies between claiming a note and
recording the outcome. Whatever finds those notes must agree with whatever claims
them, or the two disagree about what is stuck. Making the claim query itself
handle it means there is only one definition of "due".

**Cost.** The queue index must include `'indexing'`, so a note in flight is in
the index a scan walks. Migration `000005` widens it; the round-trip test asserts
the predicate's state list, because "is a partial index" does not say *which*
rows it covers.

### 9.6 Retries are capped, backed off, and then visible

**Decision.** Five attempts, doubling from one minute to a ceiling of thirty,
then the note stays `'failed'` with the error text and is never retried
automatically. `INDEX_BATCH_SIZE`, `INDEX_INTERVAL` and `INDEX_LEASE` are
configurable, and the lease must exceed the poll interval.

**Why.** An embedding runtime that is down would otherwise be retried on every
tick for the life of the deployment — a permanent background cost against a
service that cannot succeed, and one that grows with the queue. Stopping leaves
the note discoverable and its reason recorded, which is what "not searchable" has
to come with to be actionable.

**Cost.** A note that fails for a reason fixed later needs one more edit to be
picked up. Accepted: an automatic unbounded retry is the worse failure.

### 9.7 An index write that lost a race is refused, not merged

**Decision.** The state write carries `WHERE updated_at = <the note's own
updated_at>` and reports `ErrStale` when it matches nothing. Stale writes are
ignored rather than retried or reported as errors.

**Why.** An attempt that started before an edit can finish after it. Writing the
whole row would revert the edit; writing the state alone would mark the note
searchable with chunks built from text that no longer exists, and the state says
the work is done, so nothing re-queues it. Refusing leaves the newer version's own
queueing intact and the next pass rebuilds from the current text. Reporting it as
an error would log a routine concurrent edit on every edit made during an index
attempt.

**Corrected during implementation.** The first version wrote the state through
GORM and listed `updated_at` in `Select`, expecting GORM to use the note's value.
GORM maintains that column itself and stamped the current time regardless, which
would have restamped every indexed note as edited and made the guard compare a
value nothing else in the system agreed with. Both writes are now spelled out as
SQL, and the tests assert the note's own timestamp.

### 9.8 The backfill is a migration that refuses to guess

**Decision.** `000004` derives search state from the rows, using the rule in 9.2,
and is not reversible. `000003` was left schema-only after it had already been
applied anywhere.

**Why.** The column arrived with no values, and every existing note would
otherwise have been invisible to the worker: no state, no queue, no index. A
migration puts the label and the code that reads it in the same repository and in
the same order, which a startup backfill job does not.

**Why not reversible.** Downgrading would have to decide what the notes said
before, which is not recoverable from the rows. The down file runs `SELECT 1` and
explains why, because an embedded migration file cannot be empty.

**Verified.** The test stages a database with data before the backfill and asserts
the labelled rows. The predicate was briefly replaced with a plausible looser one
to confirm the test fails, because a backfill test that cannot fail is worse than
none.

### 9.9 Tenant filtering is SQL's job

**Decision.** Every search filters `user_id` in the query, and the vector index is
built on the chunk alone.

**Why.** Filtering after the fact means retrieving another user's notes and
discarding them, which leaks them into the process before the filter runs and
makes the limit wrong: `top_k` would be filled with rows belonging to somebody
else. The denormalised `user_id` in 6.4 exists for exactly this.

### 9.10 Retrieved text is data, not instruction

**Decision.** Retrieved chunks go into the planner as fenced reference material,
and the answer records one of three states: `grounded`, `no_context`, or
`ungrounded`.

**Why.** A note is user-controlled text that lands in a prompt next to
instructions. Without the fence, "ignore the plan and do X" saved as a note is an
instruction the model may follow. Grounding is recorded rather than inferred so
"the plan used my notes" and "the model made something up" are distinguishable
after the fact instead of being a matter of trust.

**Cost.** Retrieved context costs prompt budget, and a plan grounded in nothing
still has to be shown to the user rather than quietly degraded.

### 9.11 `EMBED_DIMENSIONS` is checked against the schema, not trusted

**Decision.** The configured dimension must equal 768, or startup fails.

**Why.** The column is `vector(768)` and cannot hold anything else, so a
mismatched setting cannot work — it can only fail later, on the first write, as a
database error with no explanation. pgvector will not silently truncate.

### 9.12 An update statement lists every column the service mutates

**Decision.** Repository updates spell out their columns explicitly, and a column
missing from that list is a bug even when the in-memory object is correct.

**Why.** A run is inserted before it is planned and updated once planning
finishes, so a column the update forgets keeps the default it was inserted with.
`grounding_state` and `plan_fallback` were both left out of the run update: the
plan was written from the user's own notes and every run still reported
`no_context`, because that was the column default. Nothing in the unit tests could
see it — the in-memory store keeps the pointer it was handed, so it agrees with the
planner by construction, and the plan itself looked right. An ungrounded plan that
reads like a grounded one is the exact failure this project decided to prevent in
§8.4, produced not by a small model but by a missing word in a map literal.

The live test that now guards it reads the row back from PostgreSQL after the
service has run. A fake cannot fail this way, so a fake cannot check it.

## 10. Connectors and the UI

### 10.1 Google Calendar is reached over MCP, not called in-process

**Decision.** The calendar integration is an MCP server reached through the MCP
client, not a second in-process `Tool` beside `tasks.create`.

**Why.** MCP is the last capability the Definition of Done requires, and it exists
precisely so an integration can be replaced without touching orchestration. Writing
Google in-process first would mean writing it twice, and the seam that makes the
next connector (Notion, GitHub, Linear) cheap is the one that has not been built
yet. `TaskCreateTool` was deliberately written as an in-process implementation of
the `Tool` contract so that an MCP-backed tool with the same name and argument
shape could replace it without anything above it noticing; the calendar is the
first thing to test that claim.

Consequence: the agent resolves tools by name across both sources, and the registry
has to answer for a tool it does not itself hold arguments for. That is a real
design problem, not a formality — see §10.4.

### 10.2 Only approved arguments leave the machine

**Decision.** No note content is ever sent to Google. What reaches the provider is
the exact tool payload the user saw and approved, and nothing else.

**Why.** Everything to this point has been local-first: local inference, notes in
the user's own database, retrieval fenced in SQL. A calendar integration is the
first thing that would send a user's personal knowledge to a third party, so the
line is drawn at the payload the user explicitly reviewed rather than at whatever
happens to be convenient to pass. `notes` are read to plan; they are never an
argument.

This is also why the payload stays verbatim between approval and execution, which
was already a property of the approval row (§ approval design): what is reviewed is
what runs. A connector that reconstructed the body from retrieved context at
execution time would break that guarantee while looking identical in the UI.

Enforced in code, not by convention: the calendar tool's argument decoder rejects
unknown fields, so a payload carrying note content cannot be smuggled through as an
unrecognised key. See §10.4.

### 10.3 OAuth is built as a seam before it is wired to Google

**Decision.** The token store, encryption at rest, and refresh logic are
implemented against a provider interface with a fake for tests. No real Google
credentials are configured yet.

**Why.** Storing a refresh token is the highest-consequence piece of this work: it
is a long-lived credential for a user's real account, and getting the storage
wrong is not a bug that announces itself. Building the seam first means the
threat model is decided and tested before a real secret exists on the machine.

The honest alternative — wiring real credentials now — would put a live account
credential in front of an unreviewed storage design. Credentials are a
configuration step, not a design step, so deferring them costs nothing except this
step being repeated later.

### 10.4 A connector's arguments are its own, and the planner cannot invent them

**Decision.** The planner emits a plan; a *selector* decides which tool that plan
proposes, and the tool's own validator decides whether the arguments are
executable. A plan cannot name a tool, and a tool cannot be reached without a
proposal the user approved.

**Why.** Two failure modes have to be prevented and they pull in opposite
directions. The obvious design — let the model choose the tool and the arguments —
lets it invent a call the user never intended, on a service with access to a real
account. The other extreme — hardcoding one tool per plan, which is what
`propose` does today — means a calendar connector cannot be reached by the agent at
all.

So the split is: the model proposes intent, code decides the mechanism, and the
user approves the exact bytes. The selector is deterministic and total: an
unrecognised intent produces a proposal for the default tool rather than a
best-effort guess at a mutating external service.

Argument validation stays in the tool, before the proposal is shown, so a payload
that would be refused at execution is never displayed for approval. That is the
existing rule and it extends unchanged to a tool that talks to a third party.