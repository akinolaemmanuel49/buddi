# Buddi

**Buddi** is a privacy-oriented personal life orchestrator designed to help users organize personal information, plan activities, and execute routine tasks.

The project explores how **lightweight local AI models, agents, Retrieval-Augmented Generation (RAG), tools, and the Model Context Protocol (MCP)** can work together to create a useful personal AI system.

The chat interface is the primary surface. It streams a reply token by token, keeps
the transcript so a misworded request can be revised rather than retyped, and
carries calendar writes end to end through Google's own OAuth flow.

Also in this repository: `DECISIONS.md` records the decisions made and why, and
`MVP_TODO.md` tracks what is left.

## Goals

Buddi is being built around four primary learning goals:

* **Local AI** — use lightweight open-weight models running locally.
* **RAG** — give the model access to relevant personal information without training that information into the model.
* **Agents** — use specialized agents to retrieve information, plan actions, and execute tasks.
* **Tools & MCP** — allow agents to interact with external capabilities through standardized tools.

## MVP

The MVP focuses on a single user's personal knowledge, planning, and task management.

A representative workflow:

> "Based on what you know about me, create a two-week plan for learning Kubernetes and add the resulting tasks to my task list."

```mermaid
flowchart TD
    User --> Buddi
    Buddi --> Retrieve["Retrieve personal context"]
    Retrieve --> Plan["Generate plan"]
    Plan --> Approval["User approval"]
    Approval --> Tools["Execute tools"]
    Tools --> Result["Return result"]
```

The MVP should demonstrate a complete workflow rather than simply provide a conversational interface.

## Architecture

At a high level:

```mermaid
flowchart LR
    UI["UI"] --> API["Buddi API"]
    API --> Orchestrator["Agent Orchestrator"]

    Orchestrator --> RAG["RAG"]
    Orchestrator --> LLM["Local LLM"]
    Orchestrator --> MCP["MCP Tools"]

    RAG --> DB["PostgreSQL + pgvector"]
    LLM --> Model["Local LLM"]
    MCP --> Tasks["Tasks / Other Tools"]
```

## Technology

The initial stack is:

* **Go** — API and application layer
* **Local LLM (single model)** — `qwen3:4b-instruct-2507-q4_K_M`, the only
  general-purpose model in use, at roughly 2.5 GB resident with `num_ctx 16384`

  The model was chosen by measuring candidates rather than by guessing. Two
  earlier assumptions turned out to be wrong:

  - `qwen3:0.6b` is fast but loses track of relative dates — asked for "this
    Friday" it answered with a different day entirely.
  - `qwen3:4b` *with thinking* spent 138–162 seconds and exhausted the token
    budget before producing a plan at all.

  The instruct variant of the same 4B model answers in roughly 12 seconds with
  materially better dates, which is what decided it. Smaller tiers
  (`qwen3:1.7b`, `gemma4:e2b`) were evaluated earlier and dropped: without a
  GPU the weights and the KV cache come out of the same RAM. The model is still
  chosen with the `BUDDI_MODEL` environment variable alone, so no code change is
  needed to switch. Embeddings always use `nomic-embed-text` at 768 dimensions.
* **Ollama** — initial local model runtime, containerised
* **PostgreSQL + pgvector** — application data and vector search
* **Supabase** — planned hosted PostgreSQL option
* **MCP** — tool integration
* **React + TypeScript** — chat interface, dark and light themes

The project intentionally avoids adopting a large AI framework initially. The goal is to understand the underlying concepts before introducing additional abstractions.

## Repository layout

The dependency direction is strictly inward: `api` → `application` → `domain`, with
`infrastructure` implementing ports the application declares. `domain` imports nothing
from the other three.

| Path | Holds |
| --- | --- |
| `cmd/server` | composition root; the only place all of this is wired together |
| `internal/domain` | types and rules. No behaviour that needs a database, a model or a network |
| `internal/application` | the use cases: planner, chat, agent, notes, tasks, retrieval, oauth |
| `internal/api` | HTTP: routing, decoding, mapping, SSE, error mapping |
| `internal/infrastructure` | the outside world: Postgres, Ollama, Google, MCP |
| `internal/server` | process concerns: timeouts, graceful shutdown |
| `internal/observability` | tracing, which is off unless configured |
| `buddi-web` | React chat interface |

Four things in `application` are worth finding first, because most behaviour decisions
live in them:

| Package | Decides |
| --- | --- |
| `application/planner` | what a request is for, when it needs asking about, and what date it means |
| `application/chat` | conversation shape, routing, streaming, clarification |
| `application/agent` | which tool carries an intent, and approval |
| `application/calendar` | the calendar connector's own contract |

## Running locally

Postgres, the model runtime and the observability stack all run in Docker, so
nothing needs installing on the host beyond Docker itself.

```bash
# Database only
docker compose up -d db

# Database plus tracing (Jaeger UI on http://localhost:16686)
docker compose --profile obs up -d

# Database plus the local model runtime
docker compose --profile llm up -d ollama
```

Then start the API:

```bash
cd buddi-api
cp .env.example .env      # fill in SECRET_KEY and JWT_SECRET
go run ./cmd/server
```

And the web interface, in a second terminal:

```bash
cd buddi-web
npm install
npm run dev
```

| Service | URL |
| --- | --- |
| API | http://localhost:8000 |
| Web | http://localhost:5173 |
| Ollama | http://localhost:11434 |
| Jaeger | http://localhost:16686 |

The API applies pending SQL migrations on startup. They are embedded in the
binary, so there is no separate migration step and no ordering to remember.

### Ollama

Ollama runs in a container on the `llm` profile. It is deliberately not in the
default profile, so `docker compose up -d db` never pulls a multi-gigabyte image
for someone working only on the API.

```bash
docker compose --profile llm up -d ollama

# Pull models into the container, which caches them in the ollamadata volume.
docker compose --profile llm exec ollama ollama pull qwen3:4b-instruct-2507-q4_K_M
docker compose --profile llm exec ollama ollama pull nomic-embed-text
```

Pulled models are kept in the `ollamadata` volume, so recreating the container
does not re-download them.

There is no GPU on the target machine, so the service asks for no accelerator and
inference falls back to the CPU. That shapes the configuration:

- `OLLAMA_CONTEXT_LENGTH=16384` — the KV cache grows with context, and on CPU
  context length is also the dominant latency cost.
- `OLLAMA_KV_CACHE_TYPE=q8_0` — halves KV cache memory, and it does engage on CPU.
- `OLLAMA_MAX_LOADED_MODELS=1` and `OLLAMA_NUM_PARALLEL=1` — RAM and CPU are both
  scarce, and extra resident models or parallel requests only thrash them.
- Flash attention is **not** enabled. It needs a supported accelerator, so on
  CPU it would be a setting that does nothing while implying a speed-up that never
  arrives.

A host-run API reaches the runtime on `http://127.0.0.1:11434`, the published
port. An API running as a container on the same Compose network should use
`http://ollama:11434` instead; `BUDDI_OLLAMA_BASE_URL` covers both.

CPU-only inference is slow. Measured in the `llm` service on a CPU-only machine:

- `qwen3:4b-instruct-2507-q4_K_M` plans in roughly 10–15 seconds.
- `qwen3:0.6b` runs faster but loses relative dates, which is a correctness
  failure rather than a slower success.
- `qwen3:4b` with thinking enabled took 138–162 seconds and twice exhausted the
  token budget without emitting a plan.

The service reported about 7.4 GiB of memory in total, so context length and
model size are genuinely tight. A 4B model at `num_ctx 16384` fits inside that
ceiling; the KV cache is the binding constraint, not the weights.

Ollama accepts a graded level in the same `think` field that takes a boolean.
`BUDDI_PLANNER_THINK` sets it, and **empty sends no `think` field at all**,
which is what a non-thinking instruct model needs. `think:false` cannot be
combined with `format`, so schema-constrained output has to budget for
reasoning rather than suppressing it — budget `num_predict` generously or a
plan silently comes back empty; at 512 tokens a planning call twice exhausted
the budget on reasoning and returned nothing. The planner uses 1024.

`format` must be sent as a nested JSON Schema object in the request body, not as
a string containing JSON.

To stop the runtime from consuming the whole machine, cap it after starting:

```bash
docker update --memory 6g buddi-ollama
```

### Dates are resolved in code, not by the model

This model cannot do calendar arithmetic. Asked on a Wednesday for an
appointment "on Friday at 3pm", it answered with a Thursday eight days out — and
the wrong date was written to the calendar before anything noticed.

Two things now prevent it:

1. The prompt carries an explicit table of the coming week's dates, and the
   model is told to choose a date from that table rather than to compute one.
   It is arithmetic the runtime can do exactly and the model cannot.
2. A plan for a `calendar_event` whose date falls on a different weekday than the
   request named is **rejected and retried**, with the retry prompt naming the
   correct date. This catches the case where the model picks a plausible-looking
   date from the table that is still not the day asked for.

### The check is scoped to calendar events

A deadline saying "before Friday" is allowed to land on Thursday, so rejecting on
weekday would break correct deadlines; a request naming two weekdays is skipped,
because there is no single day to check against.

### "Friday" and "next Friday" are different dates

Each weekday is listed with its next **two** occurrences, because a table holding
only the first cannot express the second:

```
Today is 2026-10-07 (Wednesday). Tomorrow is 2026-10-08 (Thursday).

Each day below lists its next two occurrences: the first is what a plain
reference to that day means, the second is what "next <day>" and "next week" mean.
  Sunday:     2026-10-11, 2026-10-18
  Monday:     2026-10-12, 2026-10-19
  ...
  Friday:     2026-10-09, 2026-10-16
```

A wrong date is rejected and retried, and the complaint names **one exact date**
rather than a day name — "the request asked for Friday" is not actionable when two
Fridays are in play.

Two failure modes, both found the hard way and now pinned by tests:

- **"Next week" resolved to the 9th instead of the 16th.** Not only a model error:
  the validator computed the next Friday and named it in the rejection, so a
  *correct* date was refused in favour of the wrong one. A check that cannot
  represent the answer is worse than no check.
- **A bare weekday stays on the soonest occurrence.** Deliberate — being one day
  out beats moving an appointment a whole week.

### Times are the user's, not the server's

The browser sends its IANA zone with every turn, and it is carried through the
plan rather than being configuration, because it is a property of the person and
not of the deployment.

This was reported as **"the calendar says 4pm when I asked for 3pm"**. The cause
was that the plan's instant was flattened to `15:00Z` and written with
`timeZone: "UTC"`. Google stores an instant and renders it in the *calendar's*
zone, so during BST the user saw 16:00 — the payload looked correct and the
calendar was not.

Three consequences, all load-bearing:

- The date table is built in the user's zone. A Friday in UTC near midnight is a
  Thursday for the user.
- The weekday check compares in the user's zone. A Thursday-evening appointment
  belongs to Friday for anyone east of Greenwich, so checking in UTC rejects the
  right date and accepts the wrong one.
- The calendar write sends a **local time with a matching zone**, so the two agree
  by construction rather than by the calendar's default happening to match.

The zone is recorded on the plan (`time_zone`), because a bare instant is
ambiguous by construction: the same number is 3pm in London and 11am in New York.

An unknown zone falls back to UTC, and a plan with no recorded zone **does not
claim to be UTC** — stating a zone not known to be true is the original bug.
Omitting it lets Google use the calendar's own, which is at least what the user
sees everywhere else.

### Which tool a request goes to

The routing table is in `planner/routing.go`, in code, because it is a decision with
known failing cases and the cases are testable:

| Kind | Examples | Goes to |
| --- | --- | --- |
| Errand | buy groceries, laundry, pick up, restock | task |
| Appointment | dentist, meeting, haircut, flight, coffee with | calendar_event |
| Appointment with no time | "I need to see the dentist" | **clarification** |

Errands are listed before appointments deliberately, so "pick up the book I
ordered for the dentist" stays an errand. A request nothing matched is reported as
not confident and left to the model.

Two details that turned out to matter:

- Matching is **whole-word**. Substring matching made `maybe` an appointment via
  `may`, and `bookstore` an errand via `buy`.
- A **clock reading counts as a time**. `dentist at 3pm` names no weekday, so a
  check that only looked for weekday names would call it untimed and ask a pointless
  question.

The table has the last word: a model that answers a routed request with a different
intent is rejected and retried, with the complaint naming the required intent.

### When something is missing, ask

An appointment with no time in it cannot be completed honestly. Rather than invent a
start time, the planner answers with `intent: clarification` and a question:

> What time should I set "I need to see the dentist" for?

That turn records **no run** — there is nothing to approve, and a run awaiting
approval for a question would leave a card with no action behind it.

The question is stored as the message content along with the **request it is about**
(`messages.clarification_request`). The request is what makes the answer usable: a
reply of "Tuesday at 4pm" names no dentist, so re-planning from it alone would lose
the subject. The marker is cleared once answered.

This is persisted rather than held in memory because the chat service is stateless
per request. Without the column, "is a question still outstanding?" would be
unanswerable after a refresh, and a reloaded thread would show the question as
answered.

The model is also asked to clarify when a named person or place is missing, and is
prompted towards questions that are answerable in a sentence — "Which doctor are
you seeing?", not "Please clarify your request".

To stop the runtime from consuming the whole machine, cap it after starting:

```bash
docker update --memory 6g buddi-ollama
```

### Telemetry

Tracing is off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set, and no exporter is
installed when it is empty. When enabled, every request span is correlated with
the request log line through the `trace_id` field, and `agent_runs.trace_id`
stores the same value so a run can be found in the tracing backend from the
database.

## Chat

Chat is the surface everything else is reached through. A turn is posted once and
the reply streams back, because a CPU-only reply takes tens of seconds and a
caller that sees nothing until the end cannot show progress or stop early.

```
POST /api/v1/chat/messages                 stream a reply (SSE)
GET  /api/v1/chat/conversations            list threads, newest first
GET  /api/v1/chat/conversations/:id        the thread
```

`mode` is `chat` or `plan`. Omitting it entirely routes automatically: calendar
wording goes to the planner, everything else stays conversational.

The event stream carries `start`, `reasoning`, `delta`, `plan`, `done` and
`error`. `reasoning` is **absent** for a model that reports none rather than
present-but-empty — an empty panel reads as broken, and a synthesised rationale
would be presented as the model's own thinking when it is not.

Validation runs before the stream opens. Opening it commits the response to
`200`, so a rejected request would otherwise be answered with a status the client
cannot act on. The server's write timeout is cleared for this route only: it is
sized for a buffered response and would otherwise cut a long reply off part way
through.

### The transcript is a tree

Messages are stored with `parent_id`, and revising a message inserts a
replacement that marks the original `superseded` rather than overwriting it. The
wording the assistant was originally given therefore survives to be read, which
is the thing worth being able to look back at.

The transcript offered to the model is budgeted and sheds its oldest turns.
Left to the runtime it truncates silently, which reads as the model forgetting.

The conversational prompt states plainly that there are no tools and that
completion must never be claimed — otherwise the model invents having saved an
event it has no way to save. It still offers drafting and summarising, so it is
not simply refusing everything.

## Google Calendar

Calendar writes go through Google's own OAuth flow. Nothing is written without a
token, and the user sees Google's consent screen.

```
GET    /api/v1/connections/google_calendar            begin the flow
DELETE /api/v1/connections/google_calendar            disconnect
GET    /api/v1/connections/google_calendar/callback   public, provider redirect
```

Set `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` and
`GOOGLE_REDIRECT_URL=http://localhost:8000/api/v1/connections/google_calendar/callback`,
plus `GOOGLE_REDIRECT_URIS` if the redirect is not already the first entry.

`BUDDI_WEB_ORIGIN` (default `http://localhost:5173`) is where the callback
redirects the browser, with the outcome in the query string.

The callback is **public and unauthenticated** — it is the one endpoint an
attacker can aim a crafted request at. The redirect therefore carries a signed
state value, and the callback rejects anything unsigned, mismatched, expired or
already used.

Tokens are encrypted at rest with `GOOGLE_TOKEN_ENCRYPTION_KEY` rather than
stored as written, because the refresh token is a long-lived credential and the
database is the part most likely to be copied or leaked.

### Google OAuth app status

This is the part that surprises people:

- **Testing** — up to 100 test users, but Google refresh tokens expire after
  **seven days**. Fine while developing; the connector appears to break on its
  own and then works again after reconnecting.
- **In production** — no seven-day expiry, but the app must be verified. For
  scopes limited to your own data that verification is just a demo video and a
  privacy policy, with no review queue in front of it.

Until it is verified, expect to reconnect roughly weekly.

### Event payload

Google rejects an event whose end is unset, and interprets a start with no
offset against a timezone the request does not name. Both are now stated
explicitly: times carry an offset, `timeZone` is `UTC`, and an event given a
start and no end is closed one hour later.

The plan's fields map onto the event directly, so the detail the user gave has
somewhere to land:

| Plan | Event | Notes |
| --- | --- | --- |
| `title` | `summary` | noun phrase: "Dentist Appointment" |
| `due_at` | `start_at` | |
| `description` | `description` | the point of the event, not its steps |
| `location` | `location` | only if the user said where |
| `attendees` | `attendees` | only the people the user named |

`steps` is **not** read on this path. It used to be the description — joined as a
numbered list — because it was the only free text a plan carried. That put
`1. Attend the dentist appointment at 3pm` into the user's calendar as the event's
description: a restatement of the request, in the field meant for what the event is.
The schema no longer requires steps for an event, and any steps a model volunteers
are dropped.

### Which fields the model fills, and which are derived

Probed against the live 4B model, this model reliably fills `title`, `due_at` and
`location`, and **never** fills `description` or `attendees`. The detail therefore
had nowhere to go and ended up in the title instead:

> `Dentist Appointment with Dr Ada Okafor`

as the event's headline.

Three attempts were made to fix that in the prompt, and the last one made things
worse:

| Attempt | Result |
| --- | --- |
| Describe the fields better | Ignored |
| Say so again, more firmly | Ignored |
| Reject the plan and name what was lost | **Worse** — the retry dumped the whole request into the title and filled *less* |

So `attendees` is derived in code when the model leaves it empty, by taking the
person the request named after "with" or "meet", stopping at the next clause so
"dinner with Sam at the Italian place" yields `Sam` and not the restaurant.

Two properties make this safe:

- It only reads words the **user typed**, so it cannot invent anyone. That is the
  failure that matters, because this text lands in somebody's real calendar.
- It is a **fallback, not an override**. A model-supplied attendee is left alone.

And it is reported in the plan's notes rather than applied silently, because a
plan that was partly derived should not look exactly like one the model produced.

`description` is deliberately **not** derived. Building a sentence from a
word-by-word scrape of the request would produce plausible-looking prose with no
way to check it, which is worse than an empty field the user can see.

### A worked example

```
Request:  "dentist appointment next Friday at 3pm with Dr Ada Okafor at Reception on Fifth Street"

Google:    Dentist Appointment
           Fri 16 Oct, 15:00 (Europe/London)
           Reception on Fifth Street
           Guests: Dr Ada Okafor
```

On failure the error includes Google's **complete raw response** alongside the
request id. Google's own messages name none of the fields that actually caused
the rejection, so without it a failed write is not actionable.

## The Agent

The run is the unit of work. It is created **before** planning starts, so a
request that dies mid-plan still leaves a record to inspect rather than a
vanished side effect.

```
POST /api/v1/agent/runs            plan a goal, return the proposed actions
POST /api/v1/agent/approvals/:id/approve
POST /api/v1/agent/approvals/:id/reject
POST /api/v1/agent/runs/:id/cancel
GET  /api/v1/agent/runs/:id
GET  /api/v1/agent/runs            list, filterable by ?status=
```

| Status | Meaning |
| --- | --- |
| `planning` | the model is working; no user action possible yet |
| `awaiting_approval` | proposed actions are waiting on a decision |
| `executing` | approved, tools are running |
| `completed` | tools finished and the result is recorded |
| `failed` | planning or execution failed; the error is stored on the run |
| `cancelled` | rejected or explicitly cancelled; nothing was applied |

Every run carries `plan_fallback`. It is `false` for a model-produced plan and
`true` when the plan is the deterministic fallback described below, so a salvaged
plan is never presented as one the model reasoned about.

Planning is answered **synchronously**: `POST /agent/runs` blocks for the model.
Measured 10–15s on CPU with `qwen3:4b-instruct-2507-q4_K_M`. That has two
consequences worth knowing:

- `BUDDI_AGENT_PLAN_TIMEOUT` (default 45s) bounds the model call, and config
  validation rejects a value at or above `REQUEST_TIMEOUT` (60s). Without that
  guard the server can abandon the response mid-plan and never record an outcome.
  A planning timeout answers `504 gateway_timeout` and leaves a `failed` run with
  the cause recorded. It deliberately does **not** fall back to a generated plan:
  the model never answered, so nothing was planned.
- The call is not idempotent. A client that times out and retries creates a
  second run, because the first may still be planning.

The run is finalised on a context detached from the request, so a client that
hangs up does not cancel the run — it still reaches `awaiting_approval`, and the
caller can pick it up from `GET /agent/runs/:id`. This is deliberate: cancelling
on disconnect would mean the work silently never happened.

Rejecting an approval cancels the whole run and applies nothing, rather than
skipping one step and continuing with the rest. Approving twice returns `409`.
Every route is scoped by user, so another user gets `404`, not `403` — the run's
existence is itself private.

An approved run executes `tasks.create` in-process through the `Tool` interface.
That interface is the seam for MCP: the orchestration around it does not change
when the implementation becomes a client. Approval is per tool call, so the same
approval record generalises to several calls without changing the endpoint shape.

One measured model weakness is visible here: because the prompt has to state the
current time, the model sometimes pastes that timestamp into the title or a
step. The prompt forbids it explicitly and the live smoke test asserts titles
stay clean, but steps still occasionally carry one.

Approval is released when a tool call fails **transiently**, so the run returns
to `awaiting_approval` and the retry the error asked for is actually possible.
A permanent rejection still consumes the approval, since re-offering it would
invite retrying something already known to be refused.

## RAG and Personal Knowledge

Buddi keeps personal information in application storage rather than embedding it into the model itself.

```mermaid
flowchart LR
    Information["Personal information"]
    Information --> Embed["Embedding model"]
    Embed --> Vector["Vector storage"]
    Query["User query"] --> Search["Semantic search"]
    Vector --> Search
    Search --> Context["Relevant context"]
    Context --> Model["Local LLM"]
    Model --> Response["Response"]
```

### How it works

Notes are embedded as they are written, chunked into ~500-token windows with 50
tokens of overlap so a sentence spanning a boundary is still retrievable from one
side of it. Each chunk carries its owner's `user_id`, and every search filters on
it in SQL rather than after the fact — filtering later would put another user's
notes in the process before discarding them, and would let `top_k` fill up with
rows belonging to somebody else.

Planning searches a user's notes for the request and passes what it finds to the
model as fenced reference material. Retrieved text is user-controlled, so it is
delivered as data with an explicit boundary rather than as instructions. Every plan
records how it was grounded:

| State | Meaning |
| --- | --- |
| `grounded` | At least one retrieved note reached the prompt |
| `no_context` | Retrieval ran and matched nothing |
| `ungrounded` | Retrieval failed and the plan was written without context |

Recording it means "the plan used your notes" and "the model invented this" stay
distinguishable after the fact, instead of being a matter of trust.

There is no public search endpoint yet. Retrieval exists to ground plans; exposing
it directly is not part of the current build.

### Staying searchable

A note that has been written is queued for indexing in the same database statement
that stores it, so a saved note cannot be silently unsearchable. The note row
carries `search_state`, the attempt count, the last error, and when it is next due.

Indexing happens inline on save, so a note is searchable by the time the response
returns, and also in a background worker in the same process. The worker's job is
what inline cannot finish: a model runtime that was down, a model still loading, or
a process that died mid-note. It claims work with a single locking statement and
holds a lease, so two workers never embed the same note, and a note whose worker
died is picked up again once its lease expires. Attempts back off from one minute
to thirty and stop after five, leaving the note marked `failed` with its reason
recorded rather than retried forever against a runtime that cannot succeed.

### Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `EMBED_DIMENSIONS` | `768` | Must match the fixed vector column width |
| `CHUNK_TOKENS` | `500` | Tokens per chunk |
| `CHUNK_OVERLAP` | `50` | Tokens repeated into the next chunk |
| `EMBED_MAX_TOKENS` | `8192` | The embedder's own input ceiling |
| `RETRIEVAL_TOP_K` | `5` | Chunks retrieved per planning request |
| `RETRIEVAL_MIN_SIMILARITY` | `0.3` | Cosine similarity floor, `-1` to `1` |
| `INDEX_BATCH_SIZE` | `20` | Notes claimed per worker pass |
| `INDEX_INTERVAL` | `5s` | Wait when the queue is empty |
| `INDEX_LEASE` | `2m` | How long a claimed note stays claimed |

`INDEX_LEASE` must exceed the slowest embedding call the runtime can make,
otherwise two workers index the same note; startup rejects a lease that is not
longer than `INDEX_INTERVAL`.

The reasoning behind these choices, including the ones that were reversed, is in
`DECISIONS.md` section 9.

## Fine-Tuning

Fine-tuning is **not part of the initial MVP**.

It may be explored later to determine whether a customized model improves Buddi-specific behavior such as structured outputs, planning, or tool selection.

Personal information should generally remain external to the model and be supplied through retrieval.

## Tests

```bash
cd buddi-api
go test ./...          # unit and integration, no external services needed
gofmt -l . && go vet ./...
```

Everything in the default run is hermetic. The database tests need the compose `db`
service, and the tests that drive a **real model** or a **real database** are gated
behind environment variables so they are skipped rather than failed on a machine that
has not been set up:

```bash
go test ./...                                             # default: no external services
BUDDI_SMOKE_OLLAMA=1 go test ./internal/application/planner/ -run TestLive -v
BUDDI_TEST_DATABASE_URI="postgres://buddi:buddi@localhost:5432/buddi_test?sslmode=disable" \
  go test ./internal/application/agent/ -run TestLive -v
```

| Gate | Enables |
| --- | --- |
| `BUDDI_SMOKE_OLLAMA=1` | model output: dates, zones, routing, event detail |
| `BUDDI_SMOKE_PLANNER=1` | planner schema and latency against the live runtime |
| `BUDDI_TEST_DATABASE_URI` | run and note-worker integration against a real Postgres |

These are not redundant with the unit tests. A fake generator proves the validator
*accepts* a correct plan and rejects a wrong one; it cannot prove the model *produces*
one. Every bug in this project that the unit tests could not see was found by a live
run — a Thursday eight days out for a Friday, a 3pm appointment arriving as `15:00Z`,
and a doctor's name copied out of a worked example in the prompt. `DECISIONS.md` records
each one.

## MVP Definition of Done

The MVP is complete when Buddi can:

1. Receive a natural-language request. — **chat surface, streamed**
2. Retrieve relevant personal information.
3. Reason over that information.
4. Generate a structured plan.
5. Present proposed actions to the user.
6. Obtain approval for mutating actions.
7. Execute an appropriate tool through MCP. — **in-process `Tool`, MCP seam in place; `tasks.create` and Google Calendar writes**
8. Persist the result.
9. Return the result to the user.
10. Provide enough execution information to debug the workflow.

The primary objective is to demonstrate a coherent **local AI + RAG + agents + tools + MCP** system.
