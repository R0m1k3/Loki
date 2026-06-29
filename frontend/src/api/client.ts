/** Petit client API typé pour le backend Loki. */

export interface OllamaStatus {
  connected: boolean;
  host: string;
  version?: string;
  error?: string;
  default_model: string;
}

export interface OllamaModel {
  name: string;
  size_go: number;
  parameter_size?: string;
  quantization?: string;
  family?: string;
}

export async function getStatus(): Promise<OllamaStatus> {
  const res = await fetch("/api/status");
  return res.json();
}

export async function getModels(): Promise<{
  models: OllamaModel[];
  default: string;
}> {
  const res = await fetch("/api/models");
  return res.json();
}

export interface Session {
  id: string;
  title: string;
  model?: string;
  created_at: number;
  updated_at: number;
  message_count?: number;
}

export interface Message {
  id: string;
  session_id: string;
  role: "user" | "assistant";
  content: string;
  model?: string;
  created_at: number;
}

export async function listSessions(): Promise<Session[]> {
  const res = await fetch("/api/sessions");
  return (await res.json()).sessions;
}

export async function createSession(model?: string): Promise<Session> {
  const res = await fetch("/api/sessions", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title: "Nouvelle session", model }),
  });
  return res.json();
}

export async function getSession(
  id: string
): Promise<{ session: Session; messages: Message[] }> {
  const res = await fetch(`/api/sessions/${id}`);
  return res.json();
}

export async function deleteSession(id: string): Promise<void> {
  await fetch(`/api/sessions/${id}`, { method: "DELETE" });
}

/** Envoie un message et streame la réponse de l'agent via SSE. */
export async function streamChat(
  body: { session_id: string; content: string; model?: string },
  handlers: {
    onToken: (t: string) => void;
    onDone: (full: string) => void;
    onError: (msg: string) => void;
  }
): Promise<void> {
  const res = await fetch("/api/chat", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.body) {
    handlers.onError("pas de flux de réponse");
    return;
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const events = buffer.split("\n\n");
    buffer = events.pop() ?? "";

    for (const block of events) {
      let event = "message";
      let data = "";
      for (const line of block.split("\n")) {
        if (line.startsWith("event: ")) event = line.slice(7).trim();
        else if (line.startsWith("data: ")) data += line.slice(6);
      }
      if (!data) continue;
      try {
        const payload = JSON.parse(data);
        if (event === "token") handlers.onToken(payload.content);
        else if (event === "done") handlers.onDone(payload.content);
        else if (event === "error") handlers.onError(payload.message);
      } catch {
        /* bloc partiel */
      }
    }
  }
}

/** Télécharge un modèle en streamant la progression via SSE. */
export async function pullModel(
  name: string,
  onProgress: (status: string, percent: number) => void
): Promise<void> {
  const res = await fetch("/api/models/pull", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  if (!res.body) return;

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split("\n\n");
    buffer = lines.pop() ?? "";
    for (const line of lines) {
      const data = line.replace(/^data: /, "").trim();
      if (!data || data === "[DONE]") continue;
      try {
        const chunk = JSON.parse(data);
        const percent =
          chunk.total && chunk.completed
            ? Math.round((chunk.completed / chunk.total) * 100)
            : 0;
        onProgress(chunk.status ?? "", percent);
      } catch {
        /* ligne partielle, ignorée */
      }
    }
  }
}
