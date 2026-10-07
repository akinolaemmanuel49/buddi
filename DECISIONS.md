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

### 2.1 `qwen3:4b-instruct-2507-q4_K_M` as the only general model

**Context.** This was originally `qwen3:0.6b`, chosen on the reasoning that the
target machine has no GPU, Ollama reported roughly 7.4 GiB in use across the
service, and a larger model would not fit alongside the embedding model in
shared RAM. That reasoning was sound and the conclusion was wrong.

**Decision.** Use `qwen3:4b-instruct-2507-q4_K_M` at `num_ctx 16384`, roughly
2.5 GB resident. Do not add a larger tier.

**Why.** The first symptom was not quality in the abstract but a *specific*
failure: `qwen3:0.6b` cannot resolve relative dates. Asked on a Wednesday for an
appointment "on Friday at 3pm" it answered with a Thursday eight days out, and
that wrong date was written to the user's calendar. A model that is fast but
silently produces a wrong date is worse than a slower model that produces a right
one, because nothing in the response signals the error.

The 4B *thinking* variant was the obvious next step and it failed too: 138–162
seconds, and twice the whole token budget spent on reasoning before any plan came
back. The instruct variant of the same 4B weights answers in roughly 12 seconds
with materially better dates. Speed was never the binding constraint —
correctness was — so the memory argument never applied.

Measured after the change: a 4B model at `num_ctx 16384` fits inside the same
7.4 GiB ceiling, because the KV cache and the embedding model are the binding
constraints rather than the weights.

**Revisit when.** A machine with a GPU, or a need this model cannot meet. Revisit
the 0.6b option only if it can be shown to produce correct dates, since that was
the entire reason it was replaced.

### 2.1a The prompt offers a date table rather than trusting the model to count

**Context.** The model cannot do calendar arithmetic. It did not matter what it
was told, because the arithmetic was still being done by the thing least able to
do it.

**Decision.** The planning prompt carries an explicit table of the coming week's
dates, and the model is instructed to *choose* a date from that table rather than
compute one. As a second line of defence, a `calendar_event` plan whose date falls
on a different weekday than the request named is rejected and retried, with the
retry prompt naming the correct date.

**Why.** This is arithmetic the runtime can do exactly and the model cannot.
Degrading to "no deadline" — what happened before — is the right call for a
*deadline*, where an absent date is merely unhelpful, but it is the wrong call
for an *event*, where an absent date means the user turns up on the wrong day.

The weekday check is scoped to calendar events for the same reason: a deadline
saying "before Friday" is allowed to land on Thursday, so checking weekday
unconditionally would reject correct deadlines. A request naming two weekdays is
skipped, because there is no single day to check against — refusing an ambiguous
request would break the conversational routing that keeps ordinary chat out of the
planner.

**Revisit when.** The model is replaced with one that handles dates reliably, at
which point this is worth removing rather than keeping as superstition.

**Known gap.** There is no user timezone. Dates are handled in UTC, so an evening
event can render in a different local day. This is tracked in `MVP_TODO.md` and
is the most likely source of a remaining "wrong date" report.

### 2.2 One model in use, not a model per stage

**Context.** Separate planner and chat models would be more flexible.

**Decision.** `PlannerModel` falls back to `Model` when unset. There is
deliberately only one general-purpose model in the config.

**Why.** Each resident model competes for the same scarce RAM, and latency
already runs 10–20 seconds per plan. A second general model would roughly halve
the number of concurrent requests the box can serve.

**Cost.** No cheap model for a hot path. Revisit if a second stage appears.

### 2.3 Think level is a setting, and empty means "do not send it"

**Context.** Ollama accepts a boolean or a graded level in the same `think` field.
With `qwen3:0.6b` the planner hardcoded `think:"low"`, chosen from this table:

| Approach | Valid output | Avg latency | Reasoning tokens |
| --- | --- | --- | --- |
| `format` + default thinking | 3/3 | 9.1s | ~359 |
| `format` + `think:"low"` | 3/3 | 5.0s | ~216 |
| Schema described in prose | 3/3 | 9.4s | included above |

**Decision.** `BUDDI_PLANNER_THINK` sets the level. **Empty sends no `think` field
at all**, which is what `qwen3:4b-instruct-2507-q4_K_M` needs.

**Why.** The hardcoded value was correct for the old model and wrong for the new
one. A non-thinking instruct model has no reasoning to cap, and the runtime
rejects `think:false` alongside `format` regardless — so the old setting was not
merely redundant but actively incompatible with the model we moved to.

`think:false` being unable to combine with `format` still holds and still matters:
schema-constrained output has to budget for reasoning rather than suppressing it.

**Why a setting rather than logic.** The planner model was already switchable by
environment variable, so a deployment that changed the model could previously not
change how it was asked to reason. Two switches for one behaviour is how a
deployment ends up with a model and a prompt that were never tested together.

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
## 11. The chat surface

The earlier shape of this repository treated the API as the product and the
interface as a scaffold. That stopped being true the moment the first end-to-end
run worked, and every decision below follows from taking the interface seriously
rather than as a demo.

### 11.1 Stream replies instead of buffering them

**Context.** A CPU-only reply takes 10–15 seconds. The buffered call worked.

**Decision.** Completions stream as newline-delimited JSON chunks, over SSE on
the HTTP surface.

**Why.** A caller that sees nothing until the end cannot show progress and cannot
stop early, so the wait is dead time from the user's point of view. It is also
unbearable repeated — every misworded request means another 15 seconds of
nothing.

Reading is done against the JSON stream rather than by splitting the payload on
newlines, because a value can contain a newline inside a string and a
line-oriented reader desynchronises on the first one it meets.

**Cost.** Two code paths can drift. The buffered and streaming requests share one
encoder so they cannot diverge on validation.

### 11.2 Reasoning is absent, never empty

**Context.** The reasoning token stream was fetched and discarded, leaving a
character count that the interface could render as "thought for 214 characters".

**Decision.** The reasoning text is carried through to the client and shown
behind a disclosure — **and the panel is omitted entirely when the model reports
no reasoning.**

**Why.** An always-present-but-empty panel reads as a bug, which trains the user
to distrust it. And there was a tempting wrong answer available: synthesising
plausible-sounding thinking to fill the panel. That would have been presented as
the model's own reasoning when it was text written by us, on a surface whose
entire purpose is showing the user what the model actually did.

A non-thinking instruct model has no reasoning to show, so absence is the honest
representation.

### 11.3 Messages form a tree, and revision preserves the original

**Context.** A run answers one question: a goal in, a plan out. There was nowhere
to put a corrected request, so a misworded message could only be abandoned and
retyped — which is exactly the loop that surfaced the wrong-date bug, because
retrying meant starting over rather than fixing one thing.

**Decision.** Messages carry `parent_id`. Revising a message inserts a
replacement and marks the original `superseded`.

**Why.** Overwriting would erase the record of what was originally asked, and that
record is the thing worth being able to look back at — especially when the
question is "why did it book a dentist appointment on a Thursday".

### 11.4 One transcript, not three ways into the same data

**Context.** The interface had a request box, a run list and a run detail pane.
All three reached the same runs.

**Decision.** A single transcript, with approvals rendered per message. The
run-centric views were deleted.

**Why.** Three entry points into one dataset stay consistent until they don't,
and they didn't: approval cards were keyed to transient state, so they flashed and
vanished, leaving a plan that nothing could be actioned on. Fixing that meant
rendering approvals from the message's own `run_id`, which is what a transcript
naturally gives you and a run list does not.

### 11.5 The transcript offered to the model is budgeted in code

**Decision.** History is trimmed oldest-first against an explicit budget before
being sent.

**Why.** The transcript grows without limit and the context window does not.
Left to the runtime it truncates silently, which surfaces as the model
appearing to forget something the user can plainly see in the conversation.

### 11.6 Routing is decided before any length limit applies

**Decision.** Calendar wording is detected without a length cap; everything else
goes through a bounded heuristic and becomes conversational.

**Why.** The failure this prevents is asymmetric. Answering a calendar request
conversationally produces the model telling the user it has saved an event, which
it has no way to do — an invented completion, on an operation that mutates
someone's real calendar. The cost of a false negative there is far higher than
the cost of planning something that could have been a chat reply.

The prompt states plainly that there are no tools and that completion must never
be claimed, and still offers drafting and summarising, so it is not simply
refusing everything.

### 11.7 Validation runs before the stream opens

**Context.** An SSE handler commits the response to `200` as soon as it starts
writing.

**Decision.** The message is validated first. The server write timeout is cleared
for the chat route only.

**Why.** A rejected request answered inside an already-open stream arrives as a
status the client cannot act on. And the write timeout is sized for a buffered
response; left in place it cuts a 15-second reply off part way through, which
presents as the stream ending with no error.

## 12. Google Calendar end to end

### 12.1 Signed, single-use OAuth state

**Decision.** The redirect carries a signed state value; the callback rejects
anything unsigned, mismatched, expired or already used.

**Why.** The callback is the one endpoint here that is public and
unauthenticated — the provider has to be able to reach it without a bearer
token. That makes it the single place an attacker can aim a crafted request, and
without state the callback will happily exchange whatever authorization code it
is handed into a token for the attacker's account.

### 12.2 Tokens encrypted at rest

**Decision.** Stored through a cipher keyed by a value supplied to the process.

**Why.** The refresh token is a long-lived credential, and the database is the
component most likely to be copied, backed up or leaked. This follows from the
decision to store the credential at all, so it is not a separate choice to
revisit.

### 12.3 Times are sent with explicit offsets and a named timezone

**Context.** Google rejected a calendar write outright.

**Decision.** Times carry an explicit offset, `timeZone` is stated, an event with
a start and no end is closed one hour later, and `endTimeUnspecified` is never
sent.

**Why.** An unset end is rejected outright. A start with no offset is
interpreted against a timezone the request never names, so the same payload means
different instants depending on an assumption made on our behalf.

**Gap.** The timezone is `UTC` because there is no user timezone yet, so an
evening event can land on the wrong local day. That is the most likely remaining
cause of a "wrong date" report and it is a real bug, not a display artifact.

### 12.4 Provider errors include the complete raw response

**Decision.** Failures surface Google's raw response body alongside the request
id.

**Why.** Google's error messages do not name the fields that caused the
rejection. Without the body, a failed calendar write is not actionable — and
because the write is user-initiated, an unfixable failure is simply a broken
feature from the user's side.

### 12.5 Approval is released on transient tool failure

**Context.** A failed approval left the run `failed` with nothing to show, and the
approval had already been spent — so the retry the error asked for was
impossible.

**Decision.** A failure that could plausibly succeed on a second attempt returns
the run and its approval to `awaiting_approval`. A permanent rejection still
consumes the approval.

**Why.** The approval exists to gate the *decision* to act, not to be a one-shot
token that a flaky network can burn. Re-offering an approval for a call already
known to be refused would invite retrying something that cannot succeed, so the
distinction is on whether the failure is likely to be different next time.

### 12.6 Repository history is reconstructed in slices

**Context.** The work accumulated in a working tree that was not under version
control.

**Decision.** Commits are grouped by concern and the two large features are
committed on branches and merged back, so there are restore points rather than
one snapshot.

**Why.** Not for the history's own sake. The same session produced two corrupted
files from broad text substitutions, which is the situation version control
exists to make recoverable, and both were caught by running the build after every
change — the discipline the commits are meant to make cheap to keep.

**Honest limitation.** Files are split at file granularity, not hunk. A file
containing more than one change is committed whole at the point the first of them
needs it, so the history is a faithful grouping of the final states rather than a
record of the order edits were typed in.
## 13. Asking instead of guessing

### 13.1 Missing information is answered with a question

**Context.** I need to see the dentist is an appointment with no time in it. The
planner had two options and both were wrong: invent a start time, or fall back to a
task titled "Dentist appointment" with no time. The second is what produced the wrong
date in the calendar in the first place, so falling back to it was not a safe default
— it was the bug, reached a different way.

**Decision.** The planner may answer with intent: clarification and a question. The
chat service records the question and no run.

**Why.** The information is held by the user and no amount of guessing substitutes for
it. Inventing a start time does not fail visibly — it produces a well-formed event on
a day the user never agreed to.

No run is recorded because there is nothing to approve. A run awaiting approval for a
question leaves a card on screen with no action behind it, which is worse than the
question alone because it looks actionable.

### 13.2 The question stores the request it is about

**Decision.** messages.clarification_request holds the original request, not the
question. The question is already the message content.

**Why.** A reply of "Tuesday at 4pm" names no dentist. Re-planning from the reply
alone loses the subject, which is the part the user is actually answering. The
question is the thing being read; the request is the thing being resumed.

The marker is cleared once answered, or the chain re-asks the same question forever —
each reply leaving a new marker behind.

### 13.3 The pending question is persisted, not remembered

**Context.** The chat service is stateless per request; the answer arrives as a later
message in a later request.

**Decision.** The outstanding-question marker is a column.

**Why.** "Is a question still outstanding?" has to be answerable from the transcript.
Holding it in memory that dies with the connection means a refresh silently converts
an outstanding question into a finished answer, and the user has no way to notice.

This is the one place the transcript tree earns a column rather than being derived: the
state is about *what the user owes us*, and it changes without a new message existing
to hang it on.

### 13.4 Which tool a request belongs to is decided in code

**Context.** uy groceries was reaching Google Calendar. The model chose the intent
from an enum whose own wording made 	ask sound like the catch-all.

**Decision.** A routing table in planner/routing.go decides the intent, and the
model is told the verdict rather than the rules. A model that answers a routed request
with a different intent is rejected and retried.

**Why.** The same reasoning as the date table: this is a decision with known failing
cases, and the failing cases are testable. Leaving it in the prompt means the cases
that already broke get fixed by rewording rather than by a test that fails when they
break again.

The cost is real and stated: a new kind of request has no rule until someone adds one.
An unmatched request is reported as not confident rather than forced into the first
row, so the table's coverage stays visible.

### 13.5 Errands are matched before appointments

**Why.** Ordering is the whole mechanism. "Pick up the book I ordered for the
dentist" is an errand that mentions an appointment, and whichever row comes first
decides it. Errands first because the failure mode is asymmetric: a stray event lands
in somebody's real calendar, while a meeting quietly recorded as a task is merely
missed.

### 13.6 Steps are not the event description

**Context.** The description in Google's calendar read 1. Attend the dentist
appointment at 3pm.

**Decision.** steps is required only of a task. The plan carries description,
location and ttendees, and EncodeCalendarArguments reads those instead.

**Why.** The description was not the model misbehaving. The encoder built it from
describeSteps(plan), because steps were the only free text a plan carried and the
connector's description was their only destination. The schema then required at
least one step of every plan, so an appointment had to invent an action to satisfy it,
and the invented action became the text in the user's calendar.

Three separate fixes, and removing only one leaves the others in place: not requiring
steps stops the invention, carrying description gives the detail somewhere to go, and
stopping the encoder reading steps removes the restatement. location and ttendees
exist for the same reason — a calendar with a separate place and a separate people list
can be searched by them, and a sentence cannot be.

### 13.7 A question nobody needed is rejected

**Decision.** A clarification on a request the table matched as actionable is rejected
and retried.

**Why.** The user asked for something and is being handed a follow-up instead. The
model will ask when it is unsure, and left unchecked that becomes a way of answering
every request with a question — technically honest, and useless.
## 14. Time

### 14.1 "Next week" is the second occurrence, not the first

**Context.** Reported: "I used next week which should be the 16th but it used the 9th."

**Decision.** The date table lists each weekday's next *two* occurrences, and the
validator resolves a weekday reference to one exact date rather than a day name.

**Why.** Part of this was the validator rather than the model. The table held seven
days, so the 16th was not in it at all, and checkWeekday computed the next Friday
and named it in the rejection. A plan dated correctly was therefore refused in
favour of the wrong one. A check that cannot represent the answer is worse than no
check, because it does not merely fail to catch the error — it produces it.

"Next" moves the target a week; a bare weekday stays on the soonest occurrence. That
asymmetry is deliberate: being one day out is a much smaller error than moving an
appointment a whole week.

The complaint names a date rather than a weekday because it goes back into the retry
prompt, and "the request asked for Friday" is not actionable when two Fridays are in
play.

**Known limit.** "In three weeks" and "next month" still reach the model. "The week
after next" is explicitly excluded from the week-shifting rule rather than being
silently treated as one week.

### 14.2 The time zone belongs to the person, not the deployment

**Context.** Reported: "the time set is 4PM instead of 3PM". The plan's instant was
15:00:00Z, written with 	imeZone: "UTC".

**Decision.** The browser's IANA zone travels with the turn, through the plan, and into
the calendar write. It is an argument rather than configuration.

**Why.** "Friday at 3pm" means Friday and three in the afternoon where the user is
standing. Configuration would make that a property of the deployment, which is wrong for
anything with more than one user and still wrong for one person who travels.

**Why it showed up as an hour.** Google stores an instant and renders it in the
*calendar's* zone. 15:00Z is 16:00 in London while BST is in force, so the payload was
correct and the calendar was not. Nothing in the write path was lying, which is why this
went uncaught until a human looked at their own calendar.

### 14.3 The zone is recorded on the plan

**Decision.** 	ime_zone on the plan, sent to the calendar alongside a local time.

**Why.** A bare instant is ambiguous by construction: the same number is 3pm in London
and 11am in New York. Recording the zone that produced it makes the stored value
interpretable after the fact, and it is what lets the write send a local time with a
matching zone so the two agree by construction rather than by the calendar's default
happening to be right.

### 14.4 Absence of a zone is not a claim of UTC

**Decision.** A plan with no recorded zone does not write a 	imezone field. An
unrecognised zone name is not written either.

**Why.** Sending "UTC" because nothing better is known states something not known to
be true, and stating a wrong zone is the original bug. Omitting the field lets Google
use the calendar's own zone, which is at least consistent with how the user sees every
other event they own.

An unknown name is dropped rather than refused because it arrives from a browser: the
realistic failure is a zone renamed in a future tzdata release, and refusing the turn
would stop the user doing anything at all. The fallback is UTC, which is wrong by at
most an hour.

### 14.5 The weekday check runs in the user's zone

**Why.** In UTC a Thursday-evening appointment belongs to Friday for anyone east of
Greenwich. Checking in UTC rejects the right date and accepts the wrong one — the same
class of bug as 14.1, and the reason the zone is threaded through the validator rather
than only through the prompt.

### 14.6 Check the encoding of what a shell writes

**Context.** Three em dashes in planner/service.go were corrupted to
U+00E2 U+20AC U+201D — an em dash's UTF-8 bytes read as CP1252 — by a PowerShell text
write.

**Why it is recorded.** The file still compiled, all tests passed, and the damage was
invisible in a diff rendered through the same console. It was found by counting non-ASCII
code points and printing them as numbers, not by reading.

The lesson is narrow and worth keeping: a tool that writes files should be checked by
something other than the tool that read them. gofmt was no help here, because
corrupted bytes inside a comment are perfectly valid Go.
### 14.7 A worked example in a prompt becomes data

**Context.** The planner prompt illustrated the description field with "Annual check-up
with Dr Ada Okafor".

**Live result.** Asked for "dentist next Friday at 3pm, it's my annual check-up" —
which names no doctor — the model answered with the title "Annual check-up with Dr Ada
Okafor". It had taken the name from the instruction.

**Why.** This is the failure already recorded in 9.10 for retrieved notes: a concrete
example in a prompt is an invitation to copy it. The mitigation there was to number the
chunks rather than label them. It was not applied here.

**Decision.** No prompt example contains a name, a place or a date. Instructions say
what to extract and add that any name, place or detail must come from the request itself.

**Why the instruction alone was not enough.** The first version of the fix did carry
"Do not put a person's name in the title" and the model stopped putting the name in the
title, which suggests it read that line. It did not stop inventing one, because nothing
told it the name had no source. Removing the example is what removed the name.

**Worth keeping in mind.** Anything a prompt names is a candidate answer. This was found
only by driving the real model; a fake generator returns whatever the test says it
returns and would never have produced it.

### 14.8 Two fields are derived, because the model will not fill them

**Context.** Live probes show the model filling title, due_at and location reliably and
leaving description and attendees empty, every time.

**Decision.** Attendees are derived from the request in code when the model leaves them
empty. Description is not derived at all.

**Why three prompt attempts did not work.** Describing the fields better: ignored.
Saying so again, more firmly: ignored. Rejecting the plan and naming the missing person
in a retry: worse — the second attempt put the whole request into the title and filled
less than the first.

That last result is the useful one. The retry mechanism is the thing that fixed the
weekday and the hour, and it fails here. Being told "the request names Ada, put her in
attendees" appears to push the model away from the fields rather than towards them, and
the plan it produced was a strictly worse answer than the one it was retrying.

So this is a capability limit, not a wording problem, and it is solved the way the other
model limits here have been: in code. The date table, the weekday check and the routing
table are all decisions the model could not make, and this is the same kind.

**Why attendees are safe to derive.** The extraction only reads words the user typed, so
it cannot invent a person, and that is the property that matters here because the result
is written to somebody's real calendar. It stops at the next clause, so "dinner with Sam
at the Italian place" yields Sam and not the restaurant. It is a fallback rather than an
override, so a model-supplied attendee is never replaced by a cruder one.

**Why description is left alone.** A description would have to be assembled from a
word-by-word scrape of the request, producing plausible-looking prose with nothing to
check it against. An empty field the user can see is better than a sentence that might
be wrong. The detail is not lost: location and attendees carry it structurally, and
Google renders both.

**Why the derived value is reported.** It appears in the plan notes. A plan that was
partly derived and one the model produced entirely look identical otherwise, and the
difference is worth being able to see.
## 15. Removing what is not used

### 15.1 Dead means unreferenced, and that has to be measured

**Context.** The repository accumulated leftovers: a two-occurrence date table replaced a
seven-day one and left its window constant behind, and a convenience constructor
survived the refactor that made its caller pass options directly.

**Decision.** Dead code is identified by reference counting, not by reading.

**Why.** Reading produces confident, wrong answers in both directions. It misses a
constant that "looks used", and it cuts something reachable only through an interface.
A count is dull but it is checkable, and it is the only thing that settles whether a
symbol is live.

**The scoping matters more than the counting.** Two mistakes produce opposite errors:

- Counting within a package flags every constructor called only from cmd/server.
  Reading the result suggests a large amount of dead code that is very much alive.
- Counting methods by name flags every interface method and every fake implementing one.
  Nothing implements an interface *by name* in Go.

So unexported symbols are counted inside their own package, because nothing outside can
reach them, and exported symbols across the module. Test entry points are excluded
because the runner calls them by name rather than by source reference.

### 15.2 A name mentioned only in its own comment is dead

**Context.** weekdayWindow and weekdayNames each appeared exactly twice: their
declaration, and a comment above that declaration describing them.

**Why this needed saying.** A naive reference count reports them as referenced. A
document that describes a symbol's purpose is not a use of it, and treating it as one
hides the exact leftovers this exercise is looking for. The count has to exclude
comments, or at least be inspected against them.

### 15.3 Interface methods and fakes are not dead code

**Decision.** Nothing implementing a port is removed for appearing unreferenced.

**Why.** ListAll, CreateEvent, ReplaceTags and the rest are referenced only by
interface satisfaction. Deleting a fake's method because the fake is only used in one
test breaks the compile in a way that reads like a bug rather than a cleanup, and
deleting the interface method itself would delete behaviour.

### 15.4 Comments are cut for being wrong, not for being long

**Decision.** Comments recording *why* stay. Comments restating what the line does go.

**Why.** The reasoning comments are the most valuable thing in the repository, and the
reason this project's model limitations were all eventually understood is that each one
was written down when it was discovered. Cutting them to make a file shorter would
trade the thing that makes the code maintainable for a cosmetic gain.

What was cut is narrower: a comment that described behaviour which no longer exists, a
doc comment whose first word did not match the identifier it documented, and duplicated
explanations of the same decision in two places.

### 15.5 Two whole-word matchers existed

**Context.** planner had containsWeekday from the weekday work, and gained
containsWord from the routing table written afterwards. Two implementations of the
same predicate, using the same boundary rule.

**Decision.** One, with the routing table's.

**Why it survived.** Neither was unused, so reference counting said both were live —
correctly. Duplication is invisible to a dead-code sweep by construction, because the
whole point of duplication is that everything is referenced.

This is the honest limit of the method: it finds what nothing uses, never what two
things both use.