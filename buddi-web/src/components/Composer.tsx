import { useEffect, useRef, useState } from "react";
import type { ChatModeOption } from "../api/client";

/**
 * Composer is the chat input.
 *
 * Enter sends and Shift+Enter breaks the line, because a box that submits on every
 * Enter cannot be used to write a request that spans two lines.
 *
 * The mode defaults to Auto rather than to Chat. Forcing Chat meant every request was
 * answered conversationally, so asking for a calendar event produced a reply claiming
 * it had been saved when nothing was. Letting the server route means the common case
 * is handled, and the toggle remains an override for the cases where it is wrong.
 */
export function Composer({
  onSend,
  busy,
  initial = "",
  placeholder = "Ask for something to track, or just talk",
  label,
  onCancel,
}: {
  onSend: (message: string, mode: ChatModeOption) => void;
  busy: boolean;
  initial?: string;
  placeholder?: string;
  label?: string;
  onCancel?: () => void;
}) {
  const [value, setValue] = useState(initial);
  const [mode, setMode] = useState<ChatModeOption>("auto");
  const areaRef = useRef<HTMLTextAreaElement | null>(null);

  const MODES: { value: ChatModeOption; label: string; hint: string }[] = [
    {
      value: "auto",
      label: "Auto",
      hint: "Let Buddi decide whether this is a question or something to track",
    },
    { value: "chat", label: "Chat", hint: "Answer conversationally, without creating anything" },
    { value: "plan", label: "Plan", hint: "Turn this into a task or event you can approve" },
  ];

  // Re-seed when the caller switches to editing a different message.
  useEffect(() => {
    setValue(initial);
  }, [initial]);

  // Grow with the content up to the CSS max-height, then scroll. Done as an effect so
  // it also applies when the value is set programmatically, such as by an edit.
  useEffect(() => {
    const area = areaRef.current;
    if (!area) {
      return;
    }
    area.style.height = "auto";
    area.style.height = `${Math.min(area.scrollHeight, 220)}px`;
  }, [value]);

  function submit() {
    const message = value.trim();

    if (!message || busy) {
      return;
    }

    onSend(message, mode);

    // Cleared here rather than by the parent, because the parent re-renders with the
    // same `initial` prop and this is the only place that knows the send happened.
    // Leaving the text in the box made it look like nothing had been submitted.
    setValue("");
  }

  return (
    <div className="composer">
      <textarea
        ref={areaRef}
        value={value}
        placeholder={placeholder}
        aria-label={label ?? "Message"}
        rows={1}
        disabled={busy}
        onChange={(e) => setValue(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey) {
            e.preventDefault();
            submit();
          }
        }}
      />

      <div className="composer-bar">
        <div className="segmented" role="group" aria-label="How to handle this message">
          {MODES.map((option) => (
            <button
              key={option.value}
              type="button"
              className={mode === option.value ? "selected" : ""}
              aria-pressed={mode === option.value}
              disabled={busy}
              onClick={() => setMode(option.value)}
              title={option.hint}
            >
              {option.label}
            </button>
          ))}
        </div>

        <span className="composer-hint">
          {mode === "auto" ? "Buddi decides whether this is a question or a request" : MODES.find((m) => m.value === mode)?.hint}
        </span>

        <div className="composer-actions">
          {busy && onCancel ? (
            <button type="button" className="quiet" onClick={onCancel}>
              Stop
            </button>
          ) : null}
          <button type="button" className="primary" onClick={submit} disabled={busy || !value.trim()}>
            Send
          </button>
        </div>
      </div>
    </div>
  );
}
