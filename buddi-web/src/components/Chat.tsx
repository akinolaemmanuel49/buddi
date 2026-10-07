import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  readCalendarOutcome,
  streamTurn,
  GOOGLE_CALENDAR,
  type CalendarOutcome,
  type ChatEvent,
  type ChatModeOption,
  type Connection,
  type Conversation,
  type Message,
} from "../api/client";
import type { Theme } from "../useTheme";
import { Composer } from "./Composer";
import { ApprovalCard, MessageBubble } from "./MessageBubble";
import { Sidebar } from "./Sidebar";

const SIDEBAR_KEY = "buddi.sidebar";

type Pending = {
  /** The assistant message id, replaced by the server's once it answers. */
  id: string;
  conversationId: string;
  /** The user's message being answered. */
  questionId: string;
  /** Held here rather than inserted into the thread, so it renders exactly once. */
  question: string;
  /**
   * How the server resolved the turn.
   *
   * Decides what is displayed while it runs: a planned turn streams the plan's raw
   * JSON, which is not text anybody wants to read mid-answer, so it is held back and
   * replaced by the finished summary.
   */
  mode: ChatModeOption;
  content: string;
  reasoning: string;
  run: { id: string; status: string } | null;
  fallback: boolean;
};

/**
 * Chat is the conversational surface.
 *
 * A turn streams, so the assistant's message is rendered from the event stream and
 * only reconciled against the server's copy when the turn ends. The user's message
 * is deliberately *not* pushed into the thread state: doing so produced a duplicate
 * bubble, because the server's `start` event renames the message to an id the
 * optimistic copy did not have, so the two stopped matching and both were drawn.
 */
export function Chat({
  userEmail,
  theme,
  onToggleTheme,
  onLogout,
}: {
  userEmail: string;
  theme: Theme;
  onToggleTheme: () => void;
  onLogout: () => void;
}) {
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);
  const [messages, setMessages] = useState<Message[]>([]);
  const [pending, setPending] = useState<Pending | null>(null);
  const [error, setError] = useState("");
  const [editing, setEditing] = useState<Message | null>(null);
  const [sidebarOpen, setSidebarOpen] = useState(() => readSidebar());
  const [connection, setConnection] = useState<Connection | null>(null);
  // The status of each run a message produced, keyed by run id.
  //
  // Held separately from the messages because it changes without them: approving a run
  // does not change any message, and reloading the thread would show a stale
  // "awaiting approval" for a decision already made.
  const [runStates, setRunStates] = useState<Record<string, string>>({});
  // The status endpoint doubles as the capability probe: a deployment with no Google
  // credentials answers 422 rather than "not connected", which is exactly the
  // difference between "you have not connected one" and "you cannot".
  const [calendarAvailable, setCalendarAvailable] = useState(false);
  const [outcome, setOutcome] = useState<CalendarOutcome>(null);

  const abortRef = useRef<AbortController | null>(null);
  const bottomRef = useRef<HTMLDivElement | null>(null);

  const loadList = useCallback(async () => {
    try {
      const list = await api.listConversations();
      setConversations(list.data);
    } catch {
      // The auth screen deals with sessions that can no longer refresh.
    }
  }, []);

  const loadConnection = useCallback(async () => {
    try {
      const status = await api.getConnection(GOOGLE_CALENDAR);
      setConnection(status);
      setCalendarAvailable(true);
    } catch {
      setCalendarAvailable(false);
      setConnection(null);
    }
  }, []);

  const openConversation = useCallback(async (id: string) => {
    abortRef.current?.abort();
    abortRef.current = null;
    setPending(null);
    setEditing(null);
    setError("");
    

    try {
      const detail = await api.getConversation(id);
      setActiveId(detail.conversation.id);
      setMessages(detail.messages);
    } catch {
      // A thread can disappear between listing and opening.
      setActiveId(null);
      setMessages([]);
    }
  }, []);

  useEffect(() => {
    loadList();

    // The OAuth callback returns the browser here, so its outcome is in the URL. It is
    // read once and stripped by readCalendarOutcome. The status is re-fetched either
    // way rather than inferred from the URL: trusting it would show "connected" for a
    // credential Google may have since refused to refresh.
    const result = readCalendarOutcome();
    if (result) {
      setOutcome(result);
    }

    loadConnection();
  }, [loadList, loadConnection]);

  useEffect(() => {
    localStorage.setItem(SIDEBAR_KEY, String(sidebarOpen));
  }, [sidebarOpen]);

  // Follow the stream, but only while something is generating: yanking the viewport
  // on every delta would fight anyone who has scrolled up to re-read something.
  useEffect(() => {
    if (pending) {
      bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
    }
  }, [pending]);

  useEffect(() => () => abortRef.current?.abort(), []);

  // Resolve the status of any run a message references, so an approval survives the
  // end of the turn that produced it.
  //
  // Fetched per run rather than embedded in the message because it changes on its own:
  // approving does not alter any message, so a thread reload would otherwise offer the
  // same decision twice.
  useEffect(() => {
    const unknown = messages
      .map((m) => m.run_id)
      .filter((id): id is string => typeof id === "string" && !(id in runStates));

    for (const runId of new Set(unknown)) {
      api
        .getRun(runId)
        .then((run) => setRunStates((prev) => ({ ...prev, [runId]: run.status })))
        .catch(() => setRunStates((prev) => ({ ...prev, [runId]: "unknown" })));
    }
  }, [messages, runStates]);

  async function send(message: string, mode: ChatModeOption, parent?: Message) {
    if (abortRef.current) {
      return;
    }

    const controller = new AbortController();
    abortRef.current = controller;

    setError("");
    setEditing(null);

    // An edit drops the branch from the edited message onwards: the turns after it
    // answered a question the user has now said was wrong, so they are removed rather
    // than left sitting above the correction's answer.
    if (parent) {
      const at = messages.findIndex((m) => m.id === parent.id);
      setMessages(at >= 0 ? messages.slice(0, at) : messages);
    }

    let answer: Pending = {
      id: "",
      conversationId: activeId ?? "",
      questionId: "",
      question: message,
      // Optimistic: a task-shaped message is assumed to be a plan until the server
      // says otherwise, so the placeholder does not flicker from "Working" to
      // "Planning" a moment in.
      mode: mode === "chat" ? "chat" : "plan",
      content: "",
      reasoning: "",
      run: null,
      fallback: false,
    };

    setPending(answer);

    // Declared outside the try: a binding introduced inside it is not visible in
    // finally, which is exactly where it is needed.
    let reconciled = false;

    try {
      const frames = await streamTurn(
        {
          message,
          // "auto" is a client-side value meaning "decide for me", so it is omitted
          // rather than sent. Sending the literal string makes the server reject the
          // turn with "mode not recognised".
          ...(mode === "auto" ? {} : { mode }),
          ...(activeId ? { conversation_id: activeId } : {}),
          ...(parent ? { parent_id: parent.id, edit: true } : {}),
        },
        controller.signal,
      );

      for await (const frame of frames) {
        if (controller.signal.aborted) {
          break;
        }

        if (frame.kind === "error") {
          setError(frame.error.message);
          continue;
        }

        if (frame.kind === "closed") {
          break;
        }

        answer = applyEvent(answer, frame.event);
        setPending({ ...answer });

        if (frame.event.type === "start" && frame.event.conversation_id) {
          setActiveId(frame.event.conversation_id);
        }
      }

      // Reconcile with the server, which owns the ids and the run link.
      if (answer.conversationId) {
        try {
          const detail = await api.getConversation(answer.conversationId);
          setMessages(detail.messages);
          setActiveId(detail.conversation.id);
          reconciled = true;
        } catch {
          // Falls through to the commit below.
        }
      }

      loadList();
    } catch (err) {
      if (!controller.signal.aborted) {
        setError(err instanceof Error ? err.message : "the request failed");
      }
    } finally {
      abortRef.current = null;

      // The streamed turn exists only in `pending`, so clearing it without keeping a
      // copy deletes the reply. That is what made a turn flash its approval and then
      // vanish: the reconciliation failed and the fallback was to throw the text away.
      if (!reconciled) {
        setMessages((prev) => commitPending(prev, answer));
      }

      setPending(null);
    }
  }

  async function decide(runId: string, approve: boolean) {
    // The approval belongs to the run and its id is not on the message, so the run is
    // read first. That is one extra request per decision, which is cheaper than
    // putting approval ids in the transcript where they would need keeping in step.
    const run = await api.getRun(runId);
    const approval = run.approvals?.[0];

    if (!approval) {
      throw new Error("there is nothing to approve on this run");
    }

    const updated = approve ? await api.approve(approval.id) : await api.reject(approval.id);

    setRunStates((prev) => ({ ...prev, [runId]: updated.status }));
  }

  function stop() {
    abortRef.current?.abort();
    abortRef.current = null;
    setPending(null);
  }

  // The question is drawn from the pending turn rather than from thread state, so it
  // cannot appear twice. Once the turn ends it comes back from the server with its
  // real id.
  const streamingQuestion = pending ? (
    <MessageBubble
      message={{
        id: pending.questionId || "pending-question",
        conversation_id: pending.conversationId,
        role: "user",
        content: pending.question,
        created_at: new Date().toISOString(),
      }}
    />
  ) : null;

  const streamingAnswer = pending ? (
    <>
      <MessageBubble
        message={{
          id: pending.id || "pending-answer",
          conversation_id: pending.conversationId,
          role: "assistant",
          content: pending.content,
          reasoning: pending.reasoning || null,
          run_id: pending.run?.id ?? null,
          created_at: new Date().toISOString(),
        }}
        streaming={!pending.run}
        reasoningAvailable={Boolean(pending.reasoning)}
        waiting={pending.mode === "plan" ? "Planning your request…" : "Working…"}
      />
      {pending.run ? (
        <RunCard runId={pending.run.id} status={pending.run.status} onDecide={decide} />
      ) : null}
    </>
  ) : null;

  const title =
    conversations.find((c) => c.id === activeId)?.title ??
    (pending ? "New chat" : "Buddi");

  return (
    <div
      className={`app ${sidebarOpen ? "sidebar-open" : "sidebar-collapsed"}`}
      style={{ position: "relative" }}
    >
      <Sidebar
        conversations={conversations}
        selectedId={activeId}
        open={sidebarOpen}
        onToggle={() => setSidebarOpen((v) => !v)}
        onOpen={openConversation}
        onNew={() => {
          setActiveId(null);
          setMessages([]);
          setPending(null);
          setEditing(null);
          
          setError("");
        }}
        onDelete={async (id) => {
          await api.deleteConversation(id);
          if (id === activeId) {
            setActiveId(null);
            setMessages([]);
          }
          loadList();
        }}
        onLogout={onLogout}
        userEmail={userEmail}
        theme={theme}
        onToggleTheme={onToggleTheme}
        connection={connection}
        calendarAvailable={calendarAvailable}
        onConnectionChanged={setConnection}
      />

      <main className="chat">
        <header className="chat-header">
          <h1>{title}</h1>
          <span className="spacer" />
        </header>

        <div className="thread-scroll">
          <div className="thread">
            {messages.length === 0 && !pending ? (
              <p className="empty-state">
                Ask for something to track and Buddi plans it, then <strong>asks before changing
                anything</strong>. Or just talk, and switch to <strong>Plan</strong> when you want
                something saved.
              </p>
            ) : null}

            {messages.map((message) => (
              <div key={message.id} className="turn">
                <MessageBubble
                  message={message}
                  onEdit={message.role === "user" && !pending ? setEditing : undefined}
                />
                {/*
                  Rendered from the message's own run, not from the turn that produced
                  it. Keying it to the transient pending state meant the card existed
                  only for the moment the turn was streaming, so an approval appeared
                  and then vanished, leaving a plan nothing could be actioned on.
                */}
                <RunCard
                  runId={message.run_id}
                  status={message.run_id ? runStates[message.run_id] : undefined}
                  onDecide={decide}
                />
              </div>
            ))}

            {streamingQuestion}
            {streamingAnswer}

            <div ref={bottomRef} />
          </div>
        </div>

        <div className="composer-wrap">
          {outcome ? <CalendarBanner outcome={outcome} onDismiss={() => setOutcome(null)} /> : null}
          {error ? <p className="error composer-error">{error}</p> : null}

          {editing ? (
            <Composer
              busy={Boolean(pending)}
              initial={editing.content}
              label="Edit your message"
              placeholder="Say it differently"
              onSend={(message, mode) => send(message, mode, editing)}
              onCancel={() => setEditing(null)}
            />
          ) : (
            <Composer
              busy={Boolean(pending)}
              onSend={(message, mode) => send(message, mode)}
              onCancel={stop}
            />
          )}
        </div>
      </main>
    </div>
  );
}

/**
 * applyEvent folds one stream event into the pending turn.
 *
 * Kept pure so each event is one obvious transition, and so it can be exercised
 * without a browser or a model.
 */
function applyEvent(pending: Pending, event: ChatEvent): Pending {
  switch (event.type) {
    case "start":
      return {
        ...pending,
        id: event.message_id ?? pending.id,
        conversationId: event.conversation_id ?? pending.conversationId,
        questionId: event.question_id ?? pending.questionId,
        mode: event.mode ?? pending.mode,
      };

    case "reasoning":
      return { ...pending, reasoning: pending.reasoning + (event.reasoning ?? "") };

    case "delta":
      // A planned turn streams the plan's JSON, which is replaced by a readable
      // summary on the plan event. Showing it in between is what made a plan look
      // like unfinished machine output.
      return pending.mode === "plan"
        ? pending
        : { ...pending, content: pending.content + (event.delta ?? "") };

    case "plan":
      // The streamed text is the raw plan JSON, which is not what a reader wants, so
      // the finished summary replaces it. The reasoning is only overwritten when the
      // server actually has some, since a plan may have produced none.
      return {
        ...pending,
        content: event.content ?? pending.content,
        reasoning: event.reasoning_complete || pending.reasoning,
        run: event.run ?? pending.run,
        fallback: Boolean(event.fallback),
      };

    case "done":
      return {
        ...pending,
        content: event.content || pending.content,
        reasoning: event.reasoning_complete || pending.reasoning,
        run: event.run ?? pending.run,
      };

    default:
      return pending;
  }
}

/**
 * commitPending keeps a streamed turn on screen when the server's copy cannot be
 * fetched.
 *
 * Without it a failed reconciliation loses the reply entirely, because the streamed
 * message is never written into thread state while it is arriving.
 */
function commitPending(messages: Message[], pending: Pending): Message[] {
  const next = [...messages];

  if (pending.questionId && !next.some((m) => m.id === pending.questionId)) {
    next.push({
      id: pending.questionId,
      conversation_id: pending.conversationId,
      role: "user",
      content: pending.question,
      created_at: new Date().toISOString(),
    });
  }

  // Only kept when there is something to show: an empty assistant bubble with nothing
  // in it is worse than no bubble, because it looks like a rendering fault.
  if ((pending.content || pending.run) && !next.some((m) => m.id === pending.id)) {
    next.push({
      id: pending.id || "pending-answer",
      conversation_id: pending.conversationId,
      role: "assistant",
      content: pending.content,
      reasoning: pending.reasoning || null,
      run_id: pending.run?.id ?? null,
      created_at: new Date().toISOString(),
    });
  }

  return next;
}

/**
 * RunCard shows the decision a planned turn left outstanding.
 *
 * Renders nothing for a message with no run, and nothing while the status is still
 * being fetched, because a card that appears a moment later is less confusing than one
 * that flashes up and is replaced.
 */
function RunCard({
  runId,
  status,
  onDecide,
}: {
  runId?: string | null;
  status?: string;
  onDecide: (runId: string, approve: boolean) => void;
}) {
  if (!runId) {
    return null;
  }

  if (status === "awaiting_approval") {
    return <ApprovalCard runId={runId} status={status} onDecide={onDecide} />;
  }

  // A settled run still gets a line: "approved" next to the plan is the only record
  // that the event reached the calendar, and without it an approved plan looks
  // identical to one that silently did nothing.
  if (status && status !== "unknown") {
    return (
      <div className="run-settled">
        <span className={`badge status-${status}`}>{status.replace(/_/g, " ")}</span>
      </div>
    );
  }

  return null;
}

function readSidebar(): boolean {
  try {
    const stored = localStorage.getItem(SIDEBAR_KEY);
    if (stored !== null) {
      return stored === "true";
    }
  } catch {
    // Storage can be unavailable; the default below still applies.
  }

  return true;
}

/**
 * CalendarBanner reports what happened at the OAuth callback.
 *
 * The reason codes come from the server and are turned into words here rather than
 * shown raw: a user who declined consent and a user whose redirect was rejected have
 * different next steps, and "access_denied" is not a next step.
 */
function CalendarBanner({ outcome, onDismiss }: { outcome: CalendarOutcome; onDismiss: () => void }) {
  if (!outcome) {
    return null;
  }

  const text =
    outcome.status === "connected"
      ? "Calendar connected. Buddi can now propose events, and will ask before writing anything."
      : failureText(outcome.reason);

  return (
    <div className={`banner ${outcome.status === "connected" ? "banner-ok" : "banner-bad"}`}>
      <span>{text}</span>
      <button type="button" className="icon" onClick={onDismiss} aria-label="Dismiss">
        ✕
      </button>
    </div>
  );
}

function failureText(reason?: string): string {
  switch (reason) {
    case "access_denied":
    case "no_code":
      return "Calendar was not connected.";
    case "unverified":
      return "That connection attempt could not be verified. Try connecting again.";
    case "exchange_failed":
      return "Google did not issue a usable credential. Try connecting again.";
    case "not_configured":
      return "This deployment has no Google credentials configured.";
    default:
      return "The calendar could not be connected.";
  }
}
