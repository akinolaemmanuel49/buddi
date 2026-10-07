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

The check is scoped to calendar events. A deadline saying "before Friday" is
allowed to land on Thursday, so rejecting on weekday would break correct
deadlines; a request naming two weekdays is skipped, because there is no single
day to check against.

Residual limitation: **there is no user timezone.** Dates and times are handled
in UTC, so an evening event can render in a different local day. Until that is
modelled, "Friday" means Friday in UTC.

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
