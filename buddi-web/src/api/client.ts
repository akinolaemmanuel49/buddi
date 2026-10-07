const TOKEN_KEY = "buddi.access_token";
const REFRESH_KEY = "buddi.refresh_token";

type Session = {
  access_token: string;
  refresh_token: string;
  token_type: string;
  expires_at: string;
  user: User;
};

export type User = {
  id: string;
  email: string;
  display_name: string;
};

type ApiErrorBody = {
  code: string;
  message: string;
  details?: unknown;
  request_id?: string;
};

export type Plan = {
  intent: string;
  title: string;
  description?: string;
  priority: string;
  due_at?: string | null;
  steps: { description: string }[];
};

export type Approval = {
  id: string;
  step_index: number;
  tool_name: string;
  arguments: unknown;
  rationale: string;
  status: string;
  created_at: string;
};

export type Run = {
  id: string;
  goal: string;
  status: string;
  plan?: Plan | null;
  approvals: Approval[];
  result?: unknown | null;
  error?: string | null;
  model?: string | null;
  trace_id?: string | null;
  plan_fallback: boolean;
  grounding_state: string;
  started_at?: string | null;
  finished_at?: string | null;
  created_at: string;
  updated_at: string;
};

export type RunList = {
  data: Run[];
  total: number;
  limit: number;
  offset: number;
};

export type Conversation = {
  id: string;
  title: string;
  head_message_id?: string | null;
  created_at: string;
  updated_at: string;
};

export type ConversationList = {
  data: Conversation[];
  total: number;
  limit: number;
  offset: number;
};

export type Message = {
  id: string;
  conversation_id: string;
  parent_id?: string | null;
  role: "user" | "assistant";
  content: string;
  reasoning?: string | null;
  run_id?: string | null;
  created_at: string;

  /**
   * This message asked a question instead of answering, and nothing has answered it
   * yet.
   *
   * Needed on reload, not just during a live stream: without it a reloaded thread
   * shows an outstanding question as an ordinary line of text, and the user has no
   * way to tell that something is waiting on them.
   */
  awaiting_answer?: boolean;
};

export type ConversationDetail = {
  conversation: Conversation;
  messages: Message[];
};

export type ChatMode = "plan" | "chat";

/**
 * Mode to send with a turn.
 *
 * "auto" is not a server-side mode: it means "omit the field" and let the service
 * decide. It is a client-side value only, which is why it is a distinct type here
 * rather than being added to ChatMode.
 */
export type ChatModeOption = ChatMode | "auto";

export type ChatEventType =
  | "start"
  | "reasoning"
  | "delta"
  | "plan"
  | "clarification"
  | "done"
  | "error";

/**
 * A question the assistant asked instead of proposing anything.
 *
 * Kept separate from a plan because the two need different things from this layer: a
 * plan wants an approval card, and a question wants a reply. Rendering one as the
 * other produces an approval card with nothing to approve.
 */
export type Clarification = {
  question: string;

  /**
   * The request the question is about.
   *
   * Shown so the user can tell which of several things in the conversation is being
   * asked about. The server re-plans against it, so a reply of "Tuesday at 4pm" still
   * arrives as a dentist appointment.
   */
  request?: string;
};

export type ChatEvent = {
  type: ChatEventType;
  conversation_id?: string;
  message_id?: string;
  question_id?: string;
  delta?: string;
  /** How the server resolved this turn. Only on the start event. */
  mode?: ChatMode;
  reasoning?: string;
  reasoning_available?: boolean;
  reasoning_complete?: string;
  content?: string;
  fallback?: boolean;
  clarification?: Clarification;
  run?: { id: string; status: string };
};

/**
 * One frame read off the stream.
 *
 * `kind` distinguishes a chat event from a failure, because the two look alike on
 * the wire: both arrive as an SSE event with a JSON body, and only one of them
 * carries the shape a chat event is expected to have.
 */
export type StreamFrame =
  | { kind: "event"; event: ChatEvent }
  | { kind: "error"; error: ApiError }
  | { kind: "closed" };

/**
 * Connection is whether a third-party account is linked to this user.
 *
 * `connected: false` is an ordinary answer, not an error: a user who has never
 * connected a calendar is the normal case, and the endpoint answers 200 either way.
 */
export type Connection = {
  provider: string;
  connected: boolean;
  scopes?: string[];
  expires_at?: string | null;
};

export type StartConnection = {
  provider: string;
  authorize_url: string;
};

/**
 * CalendarConnection names the provider Buddi integrates with.
 *
 * It is a literal rather than a variable because the UI branches on it, and a
 * typo in a connection name would silently render the wrong panel.
 */
export const GOOGLE_CALENDAR = "google_calendar" as const;

/**
 * CalendarOutcome is what the OAuth callback left in the URL.
 *
 * Read once on load and then stripped from the address, so a reload does not show a
 * stale "connected" banner for a connection that may since have been removed.
 */
export type CalendarOutcome = { status: "connected" | "error"; reason?: string } | null;

export function readCalendarOutcome(): CalendarOutcome {
  const params = new URLSearchParams(window.location.search);
  const status = params.get("calendar");

  if (status !== "connected" && status !== "error") {
    return null;
  }

  const outcome: CalendarOutcome = { status, reason: params.get("reason") ?? undefined };

  // Stripped rather than left in place: it describes one navigation, and keeping it
  // would mean every later reload reported an outcome the user is not seeing again.
  params.delete("calendar");
  params.delete("reason");

  const query = params.toString();
  window.history.replaceState(
    {},
    "",
    window.location.pathname + (query ? `?${query}` : "") + window.location.hash,
  );

  return outcome;
}

export class ApiError extends Error {
  status: number;
  code: string;
  details?: unknown;
  requestId?: string;

  constructor(status: number, body: ApiErrorBody) {
    super(body.message);
    this.status = status;
    this.code = body.code;
    this.details = body.details;
    this.requestId = body.request_id;
  }
}

export const sessionState = {
  get accessToken(): string | null {
    return localStorage.getItem(TOKEN_KEY);
  },
  get refreshToken(): string | null {
    return localStorage.getItem(REFRESH_KEY);
  },
  set(session: Session): void {
    localStorage.setItem(TOKEN_KEY, session.access_token);
    localStorage.setItem(REFRESH_KEY, session.refresh_token);
  },
  clear(): void {
    localStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(REFRESH_KEY);
  },
};

let authPromise: Promise<string | null> | null = null;

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const url = path.startsWith("/") ? path : `/api/v1/${path}`;
  const headers = new Headers(init.headers);
  headers.set("Content-Type", "application/json");

  const token = sessionState.accessToken;
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }

  let response = await fetch(url, { ...init, headers });

  if (response.status === 401 && sessionState.refreshToken && url !== "/api/v1/auth/refresh") {
    const token = await refreshOnce();
    if (token) {
      headers.set("Authorization", `Bearer ${token}`);
      response = await fetch(url, { ...init, headers });
    }
  }

  if (!response.ok) {
    let body: ApiErrorBody;
    try {
      body = (await response.json()).error as ApiErrorBody;
    } catch {
      body = { code: "http_error", message: `${response.status} ${response.statusText}` };
    }
    throw new ApiError(response.status, body);
  }

  if (response.status === 204) {
    return undefined as T;
  }

  return (await response.json()) as T;
}

async function refreshOnce(): Promise<string | null> {
  if (!authPromise) {
    authPromise = (async () => {
      const refreshToken = sessionState.refreshToken;
      if (!refreshToken) {
        return null;
      }
      try {
        const response = await fetch("/api/v1/auth/refresh", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ refresh_token: refreshToken }),
        });
        if (!response.ok) {
          sessionState.clear();
          return null;
        }
        const session = (await response.json()) as Session;
        sessionState.set(session);
        return session.access_token;
      } catch {
        return null;
      } finally {
        authPromise = null;
      }
    })();
  }
  return authPromise;
}

export const api = {
  register(display_name: string, email: string, password: string): Promise<Session> {
    return request<Session>("auth/register", {
      method: "POST",
      body: JSON.stringify({ email, password, display_name }),
    }).then((session) => {
      sessionState.set(session);
      return session;
    });
  },

  login(email: string, password: string): Promise<Session> {
    return request<Session>("auth/login", {
      method: "POST",
      body: JSON.stringify({ email, password }),
    }).then((session) => {
      sessionState.set(session);
      return session;
    });
  },

  logout(): Promise<void> {
    const refreshToken = sessionState.refreshToken;
    if (refreshToken) {
      return request<void>("auth/logout", {
        method: "POST",
        body: JSON.stringify({ refresh_token: refreshToken }),
      }).finally(() => sessionState.clear());
    }
    sessionState.clear();
    return Promise.resolve();
  },

  me(): Promise<User> {
    return request<User>("auth/me");
  },

  createRun(goal: string): Promise<Run> {
    return request<Run>("agent/runs", {
      method: "POST",
      body: JSON.stringify({ goal }),
    });
  },

  listRuns(limit = 20): Promise<RunList> {
    return request<RunList>(`agent/runs?limit=${limit}`);
  },

  getRun(id: string): Promise<Run> {
    return request<Run>(`agent/runs/${id}`);
  },

  cancelRun(id: string): Promise<Run> {
    return request<Run>(`agent/runs/${id}/cancel`, { method: "POST" });
  },

  approve(id: string): Promise<Run> {
    return request<Run>(`agent/approvals/${id}/approve`, { method: "POST" });
  },

  reject(id: string): Promise<Run> {
    return request<Run>(`agent/approvals/${id}/reject`, { method: "POST" });
  },

  listConversations(limit = 30): Promise<ConversationList> {
    return request<ConversationList>(`chat/conversations?limit=${limit}`);
  },

  getConversation(id: string): Promise<ConversationDetail> {
    return request<ConversationDetail>(`chat/conversations/${id}`);
  },

  deleteConversation(id: string): Promise<void> {
    return request<void>(`chat/conversations/${id}`, { method: "DELETE" });
  },

  /** Whether a provider is linked. Resolves to connected:false rather than rejecting. */
  getConnection(provider: string): Promise<Connection> {
    return request<Connection>(`connections/${provider}`);
  },

  /** Begins an authorization: the URL to send the browser to. */
  startConnection(provider: string): Promise<StartConnection> {
    return request<StartConnection>(`connections/${provider}`, { method: "POST" });
  },

  deleteConnection(provider: string): Promise<void> {
    return request<void>(`connections/${provider}`, { method: "DELETE" });
  },
};

/**
 * The browser's own IANA time zone, or undefined when it cannot be determined.
 *
 * Sent with every turn because "Friday at 3pm" means Friday and three in the afternoon
 * where the user is standing. Without it the server works in UTC, which names the wrong
 * day near midnight and writes an instant the calendar then displays an hour out — a 3pm
 * appointment appearing as 4pm for anyone east of Greenwich.
 *
 * Undefined rather than a guess when unavailable: the server treats an absent zone as
 * UTC, and inventing "Europe/London" would be a worse failure than the honest default.
 */
export function browserTimeZone(): string | undefined {
  try {
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    return zone || undefined;
  } catch {
    return undefined;
  }
}

/**
 * streamTurn posts a message and yields each server-sent event as it arrives.
 *
 * It is a fetch stream rather than EventSource because EventSource cannot send an
 * Authorization header, so it would have to put the access token in the URL. It also
 * cannot be aborted cleanly once a request is in flight, and abandoning a generation
 * mid-answer is the normal way to stop one on a slow runtime.
 */
export async function streamTurn(
  body: {
    message: string;
    conversation_id?: string;
    parent_id?: string;
    mode?: ChatModeOption;
    edit?: boolean;
  },
  signal: AbortSignal,
): Promise<AsyncGenerator<StreamFrame>> {
  const url = "/api/v1/chat/messages";
  const headers = new Headers({ "Content-Type": "application/json" });

  const token = sessionState.accessToken;
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }

  const payload = { ...body, time_zone: browserTimeZone() };

  // A 401 here is retried through the shared refresh, because a stream that fails
  // on an expired token would otherwise log the user out mid-sentence.
  let response = await fetch(url, {
    method: "POST",
    headers,
    body: JSON.stringify(payload),
    signal,
  });

  if (response.status === 401 && sessionState.refreshToken) {
    const refreshed = await refreshOnce();
    if (refreshed) {
      headers.set("Authorization", `Bearer ${refreshed}`);
      response = await fetch(url, {
        method: "POST",
        headers,
        body: JSON.stringify(payload),
        signal,
      });
    }
  }

  if (!response.ok || !response.body) {
    let errorBody: ApiErrorBody;
    try {
      errorBody = (await response.json()).error as ApiErrorBody;
    } catch {
      errorBody = { code: "http_error", message: `${response.status} ${response.statusText}` };
    }
    throw new ApiError(response.status, errorBody);
  }

  return readEventStream(response.body);
}

/**
 * readEventStream parses an SSE body one event at a time.
 *
 * Written against the raw byte stream rather than split on newlines because a JSON
 * payload can contain a newline inside a string, and a line-oriented reader would
 * desynchronise on the first one it met and emit nonsense from there on.
 *
 * Aborting the fetch that produced the body is what ends this: the reader's pending
 * read rejects, so there is no separate cancellation to thread through.
 */
async function* readEventStream(body: ReadableStream<Uint8Array>): AsyncGenerator<StreamFrame> {
  const reader = body.getReader();
  const decoder = new TextDecoder();

  let name = "";
  let data = "";

  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) {
        break;
      }

      const chunk = decoder.decode(value, { stream: true });

      // Split on newlines but keep the trailing newline, since a bare "\n" is the
      // event terminator and dropping it would merge two events into one.
      const lines = chunk.match(/[^\n]*\n|[^\n]+$/g) ?? [];

      for (const raw of lines) {
        const line = raw.replace(/\r?\n$/, "");

        if (line === "") {
          // A blank line terminates an event. A stream that closes mid-event, which a
          // dropped connection does, still yields whatever had accumulated.
          if (name !== "" && data !== "") {
            const frame = parseFrame(name, data);
            if (frame) {
              yield frame;
            }
          }
          name = "";
          data = "";
          continue;
        }

        if (line.startsWith(":")) {
          // A comment, which the server sends to keep the connection warm.
          continue;
        }

        if (line.startsWith("event:")) {
          name = line.slice(6).trim();
          continue;
        }

        if (line.startsWith("data:")) {
          data += line.slice(5).replace(/^ /, "");
        }
      }
    }

    if (name !== "" && data !== "") {
      const frame = parseFrame(name, data);
      if (frame) {
        yield frame;
      }
    }

    yield { kind: "closed" };
  } finally {
    reader.cancel().catch(() => undefined);
  }
}

function parseFrame(name: string, data: string): StreamFrame | null {
  let parsed: unknown;

  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }

  const payload = parsed as ChatEvent & { error?: ApiErrorBody };

  if (name === "error") {
    return {
      kind: "error",
      error: new ApiError(500, payload.error ?? { code: "stream_error", message: "the turn failed" }),
    };
  }

  return { kind: "event", event: { ...payload, type: name as ChatEvent["type"] } };
}
