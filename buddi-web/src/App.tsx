import { useEffect, useState } from "react";
import { api, sessionState, type User } from "./api/client";
import { Chat } from "./components/Chat";
import { Login } from "./components/Login";
import { useTheme } from "./useTheme";

export function App() {
  const [user, setUser] = useState<User | null>(null);
  const [authed, setAuthed] = useState(() => Boolean(sessionState.accessToken));
  const [theme, toggleTheme] = useTheme();

  useEffect(() => {
    if (!authed) {
      return;
    }

    api.me().then(setUser).catch(() => setAuthed(false));
  }, [authed]);

  if (!authed) {
    return (
      <Login onAuthed={() => setAuthed(true)} theme={theme} onToggleTheme={toggleTheme} />
    );
  }

  return (
    <Chat
      userEmail={user?.display_name ?? user?.email ?? "You"}
      theme={theme}
      onToggleTheme={toggleTheme}
      onLogout={async () => {
        await api.logout();
        setUser(null);
        setAuthed(false);
      }}
    />
  );
}
