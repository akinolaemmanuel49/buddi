import { useState } from "react";
import type { Message } from "../api/client";

/**
 * ReasoningPanel shows what the model said it was doing.
 *
 * Collapsed by default: reasoning is supporting detail, and an always-open panel
 * buries the answer it belongs to. It returns nothing when there is no reasoning,
 * which is the normal case for a non-thinking model and is not an error.
 */
export function ReasoningPanel({ text, live }: { text: string; live: boolean }) {
  const [open, setOpen] = useState(false);

  if (!text) {
    return null;
  }

  return (
    <div className="reasoning">
      <button type="button" className="reasoning-toggle" onClick={() => setOpen((v) => !v)}>
        <span>{open ? "Hide reasoning" : "Show reasoning"}</span>
        {live ? <span className="pulse" aria-label="still generating" /> : null}
      </button>
      {open ? <pre className="reasoning-body">{text}</pre> : null}
    </div>
  );
}

/**
 * ApprovalCard offers the decision a planned turn produced.
 *
 * It is a separate card under the message rather than part of it, because approving
 * is not part of reading a reply: it is a distinct, deliberate act on a run.
 */
export function ApprovalCard({
  runId,
  status,
  onDecide,
}: {
  runId: string;
  status: string;
  onDecide: (runId: string, approve: boolean) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const settled = status !== "awaiting_approval";

  async function decide(approve: boolean) {
    setBusy(true);
    setError("");
    try {
      await onDecide(runId, approve);
    } catch (err) {
      setError(err instanceof Error ? err.message : "the decision could not be recorded");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="approval">
      <div className="approval-head">
        <span className="badge">run {runId.slice(0, 8)}</span>
        <span className={`badge status-${status}`}>{status.replace(/_/g, " ")}</span>
        <span className="spacer" />
      </div>

      {settled ? null : (
        <div className="approval-actions">
          <button type="button" className="danger" disabled={busy} onClick={() => decide(false)}>
            Reject
          </button>
          <button type="button" className="primary" disabled={busy} onClick={() => decide(true)}>
            Approve
          </button>
        </div>
      )}

      {error ? <p className="error">{error}</p> : null}
    </div>
  );
}

/**
 * MessageBubble renders one turn.
 *
 * A user's message can be edited, which is the reason this surface exists: a run
 * could be rejected but not corrected, so a request that was subtly wrong could only
 * be abandoned and retyped from scratch.
 */
export function MessageBubble({
  message,
  streaming,
  reasoningAvailable,
  waiting,
  onEdit,
}: {
  message: Message;
  streaming?: boolean;
  reasoningAvailable?: boolean;
  /** What to say before the first token arrives. */
  waiting?: string;
  onEdit?: (message: Message) => void;
}) {
  const isUser = message.role === "user";

  return (
    <article
      className={`bubble ${isUser ? "from-user" : "from-assistant"} ${
        message.awaiting_answer ? "is-question" : ""
      }`}
    >
      <div className="bubble-role">
        {isUser ? "You" : message.awaiting_answer ? "Buddi is asking" : "Buddi"}
      </div>

      <div className="bubble-body">
        {/* Before the first token there is nothing to show, so the bubble says which
            of the two waiting states it is in rather than rendering an empty box. */}
        {message.content ? (
          <>
            {message.content}
            {streaming ? <span className="caret" aria-label="generating" /> : null}
          </>
        ) : streaming ? (
          <>
            {reasoningAvailable ? "Reasoning…" : (waiting ?? "Working…")}
            <span className="caret" aria-label="generating" />
          </>
        ) : null}
      </div>

      {message.reasoning ? (
        <ReasoningPanel text={message.reasoning} live={Boolean(streaming)} />
      ) : null}

      {isUser && onEdit ? (
        <div className="bubble-actions">
          <button type="button" className="quiet" onClick={() => onEdit(message)}>
            Edit
          </button>
        </div>
      ) : null}
    </article>
  );
}
