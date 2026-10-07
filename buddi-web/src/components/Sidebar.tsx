import type { Connection, Conversation } from "../api/client";
import type { Theme } from "../useTheme";
import { CalendarPanel } from "./CalendarPanel";

/**
 * Sidebar holds the thread list.
 *
 * It collapses to nothing rather than to a narrow strip, because a 60px column of
 * truncated titles is neither a list nor gone. The toggle stays reachable as a rail
 * button when collapsed, so reopening never requires a page reload.
 */
export function Sidebar({
  conversations,
  selectedId,
  open,
  onToggle,
  onOpen,
  onNew,
  onDelete,
  onLogout,
  userEmail,
  theme,
  onToggleTheme,
  connection,
  calendarAvailable,
  onConnectionChanged,
}: {
  conversations: Conversation[];
  selectedId: string | null;
  open: boolean;
  onToggle: () => void;
  onOpen: (id: string) => void;
  onNew: () => void;
  onDelete: (id: string) => void;
  onLogout: () => void;
  userEmail: string;
  theme: Theme;
  onToggleTheme: () => void;
  connection: Connection | null;
  calendarAvailable: boolean;
  onConnectionChanged: (connection: Connection) => void;
}) {
  return (
    <>
      <button
        type="button"
        className="icon rail"
        onClick={onToggle}
        aria-label="Show chats"
        aria-expanded={open}
        title="Show chats"
      >
        ☰
      </button>

      <aside className="sidebar">
        <div className="sidebar-inner">
          <div className="sidebar-top">
            <div className="brand">
              <span className="brand-name">Buddi</span>
              <span className="identity">{userEmail}</span>
            </div>
            <div style={{ display: "flex", gap: 2 }}>
              <button
                type="button"
                className="theme-toggle"
                onClick={onToggleTheme}
                aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} theme`}
                title={`Switch to ${theme === "dark" ? "light" : "dark"} theme`}
              >
                {theme === "dark" ? "☀" : "☾"}
              </button>
              <button
                type="button"
                className="icon"
                onClick={onToggle}
                aria-label="Hide chats"
                aria-expanded={open}
                title="Hide chats"
              >
                ⟨
              </button>
            </div>
          </div>

          <div className="sidebar-actions">
            <button type="button" className="primary" onClick={onNew}>
              New chat
            </button>
            <button type="button" className="quiet" onClick={onLogout}>
              Log out
            </button>
          </div>

          {conversations.length === 0 ? (
            <p className="identity">No chats yet.</p>
          ) : (
            <ul className="thread-list">
              {conversations.map((conversation) => (
                <li key={conversation.id} className="thread-row">
                  <button
                    type="button"
                    className={`thread-open ${conversation.id === selectedId ? "selected" : ""}`}
                    onClick={() => onOpen(conversation.id)}
                  >
                    <span className="thread-title">{conversation.title}</span>
                    <span className="thread-time">
                      {new Date(conversation.updated_at).toLocaleString()}
                    </span>
                  </button>
                  <button
                    type="button"
                    className="quiet icon thread-delete"
                    onClick={() => onDelete(conversation.id)}
                    aria-label={`Delete ${conversation.title}`}
                    title="Delete chat"
                  >
                    ✕
                  </button>
                </li>
              ))}
            </ul>
          )}

          {/*
            Pinned below the thread list rather than floating over the thread: it is a
            setting, not something to read mid-conversation, but it should not need a
            menu to find either.
          */}
          <CalendarPanel
            connection={connection}
            available={calendarAvailable}
            onChanged={onConnectionChanged}
          />
        </div>
      </aside>
    </>
  );
}
