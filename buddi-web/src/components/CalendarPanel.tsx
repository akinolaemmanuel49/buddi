import { useState } from "react";
import { GOOGLE_CALENDAR, api, type Connection } from "../api/client";

/**
 * CalendarPanel shows whether Google Calendar is connected and offers the two actions.
 *
 * Connect navigates the whole tab to Google rather than opening a popup. A popup was
 * the obvious alternative and is the wrong one here: the OAuth callback carries no
 * session, so the popup cannot tell the app what happened and the app would have to
 * poll for it. A real navigation returns to the app by itself, with the outcome in the
 * URL, which is why the callback redirects rather than answering with JSON.
 */
export function CalendarPanel({
  connection,
  available,
  onChanged,
}: {
  connection: Connection | null;
  /** False when the deployment has no Google credentials, so connecting cannot work. */
  available: boolean;
  onChanged: (connection: Connection) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const connected = Boolean(connection?.connected);

  async function connect() {
    setBusy(true);
    setError("");

    try {
      const start = await api.startConnection(GOOGLE_CALENDAR);
      window.location.assign(start.authorize_url);
    } catch (err) {
      setError(err instanceof Error ? err.message : "could not start the connection");
      setBusy(false);
    }
  }

  async function disconnect() {
    setBusy(true);
    setError("");

    try {
      await api.deleteConnection(GOOGLE_CALENDAR);
      onChanged({ provider: GOOGLE_CALENDAR, connected: false });
    } catch (err) {
      setError(err instanceof Error ? err.message : "could not disconnect");
    } finally {
      setBusy(false);
    }
  }

  if (!available) {
    return null;
  }

  return (
    <div className="calendar-panel">
      <div className="calendar-head">
        <span className="calendar-title">Google Calendar</span>
        <span className={`badge ${connected ? "status-completed" : ""}`}>
          {connected ? "Connected" : "Not connected"}
        </span>
      </div>

      {connected ? (
        <>
          <p className="calendar-note">
            Buddi can put events on your calendar. It always asks before writing anything.
          </p>
          <button type="button" className="quiet" disabled={busy} onClick={disconnect}>
            Disconnect
          </button>
        </>
      ) : (
        <>
          <p className="calendar-note">
            Connect a calendar and Buddi can add events to it, after asking you first.
          </p>
          <button type="button" className="primary" disabled={busy} onClick={connect}>
            {busy ? "Opening Google…" : "Connect calendar"}
          </button>
        </>
      )}

      {error ? <p className="error">{error}</p> : null}
    </div>
  );
}