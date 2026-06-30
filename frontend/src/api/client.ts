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

export interface ToolCall {
  name: string;
  args: Record<string, unknown>;
  summary?: string;
  status?: "ok" | "error" | "running" | "pending";
}

export interface Message {
  id: string;
  session_id: string;
  role: "user" | "assistant";
  content: string;
  model?: string;
  meta?: { tools?: ToolCall[] } | null;
  created_at: number;
}

export interface AgentConfig {
  system_prompt: string;
  temperature: number;
  top_p: number;
  top_k: number;
  max_tokens: number;
  num_ctx: number;
  num_gpu: number;
  num_batch: number;
  tools: Record<string, boolean>;
  confirm_shell: boolean;
}

export async function runShell(
  command: string
): Promise<{ command: string; exit_code: number; output: string }> {
  const res = await fetch("/api/shell/run", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ command }),
  });
  return res.json();
}

export async function getConfig(model?: string): Promise<{
  config: AgentConfig;
  available_tools: string[];
}> {
  const query = model ? `?model=${encodeURIComponent(model)}` : "";
  const res = await fetch(`/api/config${query}`);
  return res.json();
}

export async function saveConfig(
  patch: Partial<AgentConfig>,
  model?: string
): Promise<AgentConfig> {
  const query = model ? `?model=${encodeURIComponent(model)}` : "";
  const res = await fetch(`/api/config${query}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  return (await res.json()).config;
}

export interface FileNode {
  name: string;
  path: string;
  type: "dir" | "file";
  size?: number;
  children?: FileNode[];
}

export async function listFiles(): Promise<FileNode[]> {
  const res = await fetch("/api/files");
  return (await res.json()).tree;
}

export async function fileContent(path: string): Promise<string> {
  const res = await fetch(`/api/files/content?path=${encodeURIComponent(path)}`);
  if (!res.ok) return "";
  return (await res.json()).content;
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
    onToolCall: (call: ToolCall) => void;
    onToolResult: (call: ToolCall) => void;
    onToolConfirm: (command: string) => void;
    onStatus: (msg: string) => void;
    onNotice: (msg: string) => void;
    onDone: (full: string) => void;
    onError: (msg: string) => void;
  }
): Promise<void> {
  let res: Response;
  try {
    res = await fetch("/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch (err) {
    const raw = err instanceof Error ? err.message : "serveur Loki injoignable";
    handlers.onError(
      /network error|failed to fetch/i.test(raw)
        ? "Serveur Loki injoignable. Vérifiez le reverse proxy et le conteneur."
        : raw
    );
    return;
  }
  if (!res.ok) {
    let message = `requête refusée (${res.status})`;
    try {
      const payload = await res.json();
      message = payload.detail ?? payload.error ?? message;
    } catch {
      /* réponse non JSON */
    }
    handlers.onError(message);
    return;
  }
  if (!res.body) {
    handlers.onError("pas de flux de réponse");
    return;
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let terminal = false;
  let failed = false;

  const dispatch = (raw: string) => {
    const block = raw.replace(/\r\n/g, "\n");
    let event = "message";
    const dataLines: string[] = [];
    for (const line of block.split("\n")) {
      if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) dataLines.push(line.slice(5).trimStart());
    }
    if (dataLines.length === 0) return;
    try {
      const payload = JSON.parse(dataLines.join("\n"));
      if (event === "token") handlers.onToken(payload.content);
      else if (event === "status") handlers.onStatus(payload.message);
      else if (event === "notice") handlers.onNotice(payload.message);
      else if (event === "tool_call")
        handlers.onToolCall({ ...payload, status: "running" });
      else if (event === "tool_result") handlers.onToolResult(payload);
      else if (event === "tool_confirm") handlers.onToolConfirm(payload.command);
      else if (event === "error") {
        failed = true;
        handlers.onError(payload.message);
      } else if (event === "done") {
        terminal = true;
        if (payload.error) {
          if (!failed) handlers.onError(payload.error);
          failed = true;
        } else if (!failed) {
          handlers.onDone(payload.content);
        }
      }
    } catch {
      if (!failed) {
        failed = true;
        handlers.onError("réponse illisible reçue du serveur");
      }
    }
  };

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, "\n");
      const events = buffer.split("\n\n");
      buffer = events.pop() ?? "";
      for (const block of events) {
        if (block.trim()) dispatch(block);
      }
    }
    buffer += decoder.decode();
    if (buffer.trim()) dispatch(buffer);
  } catch (err) {
    if (!failed) {
      failed = true;
      const raw = err instanceof Error ? err.message : "connexion interrompue";
      handlers.onError(
        /network error|failed to fetch/i.test(raw)
          ? "Connexion interrompue pendant le chargement du modèle. Vérifiez OLLAMA_HOST et le délai du reverse proxy."
          : raw
      );
    }
  }

  if (!terminal && !failed) {
    handlers.onError("le serveur a fermé la réponse avant sa fin");
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
