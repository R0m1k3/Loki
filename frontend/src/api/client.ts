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

export interface GpuStats {
  name: string;
  util_pct: number;
  vram_used_mb: number;
  vram_total_mb: number;
}

export interface SystemStats {
  cpu_pct: number;
  ram_used_go: number;
  ram_total_go: number;
  ram_pct: number;
  gpu: GpuStats | null;
}

export async function getSystemStats(): Promise<SystemStats> {
  const res = await fetch("/api/system/stats");
  return res.json();
}

/** Battement groupé : statut + ressources + modèles chargés en UNE requête. */
export interface Pulse {
  status: OllamaStatus;
  stats: SystemStats;
  loaded: LoadedModel[];
}

export async function getPulse(): Promise<Pulse> {
  const res = await fetch("/api/system/pulse");
  if (!res.ok) throw new Error(`pulse ${res.status}`);
  return res.json();
}

// ── Git du workspace ─────────────────────────────────────────────────────
export interface GitCommit {
  hash: string;
  full_hash: string;
  subject: string;
  author: string;
  when: string;
  files_changed: number;
}

const projQuery = (project?: string | null) =>
  project ? `&project=${encodeURIComponent(project)}` : "";

export async function getGitLog(project?: string | null): Promise<GitCommit[]> {
  try {
    const res = await fetch(`/api/git/log?limit=40${projQuery(project)}`);
    return (await res.json()).commits;
  } catch {
    return [];
  }
}

export async function getGitDiff(
  hash?: string,
  project?: string | null
): Promise<string> {
  const params = new URLSearchParams();
  if (hash) params.set("hash", hash);
  if (project) params.set("project", project);
  const qs = params.toString();
  const res = await fetch(`/api/git/diff${qs ? `?${qs}` : ""}`);
  if (!res.ok) return "";
  return (await res.json()).diff;
}

export async function revertCommit(
  hash: string,
  project?: string | null
): Promise<{ message: string }> {
  const res = await fetch("/api/git/revert", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ hash, project: project ?? null }),
  });
  if (!res.ok) throw await apiError(res, "revert impossible");
  return res.json();
}

export interface HardwareInfo {
  loki_gpus: {
    index: number;
    name: string;
    vram_total_mb: number;
    vram_used_mb: number;
    util_pct: number;
  }[];
  gpu_override: { name: string; vram_total_mb: number } | null;
  ollama: {
    host: string;
    connected: boolean;
    version?: string;
    error?: string;
    running: {
      name: string;
      processor: string;
      gpu_percent: number;
      size_mb: number;
      vram_mb: number;
    }[];
  };
  ollama_is_local: boolean;
  note: string;
}

export async function getHardware(): Promise<HardwareInfo> {
  const res = await fetch("/api/system/hardware");
  return res.json();
}

export async function getVersion(): Promise<string> {
  try {
    const res = await fetch("/api/version");
    return (await res.json()).version ?? "?";
  } catch {
    return "?";
  }
}

export interface LoadedModel {
  name: string;
  on_gpu: boolean;
  gpu_percent: number;
}

async function apiError(res: Response, fallback: string): Promise<Error> {
  try {
    const payload = await res.json();
    return new Error(payload?.detail ?? payload?.error ?? fallback);
  } catch {
    return new Error(fallback);
  }
}

interface WarmStatus {
  state: "idle" | "loading" | "loaded" | "error";
  error?: string;
  processor?: "gpu" | "cpu" | "mixte";
  gpu_percent?: string;
}

const wait = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * Lance le préchargement en arrière-plan puis suit son état. Chaque requête
 * reste courte afin qu'un reverse proxy ne puisse plus interrompre le warm-up.
 */
export async function warmModel(
  name: string,
  keepAlive = "30m"
): Promise<WarmStatus> {
  const res = await fetch("/api/models/warm", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, keep_alive: keepAlive }),
  });
  if (!res.ok) {
    throw await apiError(res, `préchargement refusé (${res.status})`);
  }

  // Backoff progressif : réactif au début (modèle déjà chargé), espacé ensuite
  // pour ne pas marteler l'API pendant un long chargement.
  const deadline = Date.now() + 10 * 60 * 1000;
  let delay = 1000;
  while (Date.now() < deadline) {
    const statusRes = await fetch(
      `/api/models/warm/status?name=${encodeURIComponent(name)}`,
      { cache: "no-store" }
    );
    if (!statusRes.ok) {
      throw await apiError(statusRes, `suivi du préchargement refusé (${statusRes.status})`);
    }
    const status = (await statusRes.json()) as WarmStatus;
    if (status.state === "loaded") return status;
    if (status.state === "error") {
      throw new Error(status.error ?? "préchargement impossible");
    }
    await wait(delay);
    delay = Math.min(delay * 1.5, 5000);
  }
  throw new Error("préchargement toujours en cours après 10 minutes");
}

export async function getLoadedModels(): Promise<LoadedModel[]> {
  try {
    const res = await fetch("/api/models/loaded");
    return (await res.json()).loaded;
  } catch {
    return [];
  }
}

export async function getModels(): Promise<{
  models: OllamaModel[];
  default: string;
}> {
  const res = await fetch("/api/models");
  return res.json();
}

export async function deleteModel(name: string): Promise<void> {
  const res = await fetch("/api/models", {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) {
    const payload = await res.json().catch(() => null);
    throw new Error(payload?.detail ?? `suppression refusée (${res.status})`);
  }
}

export interface Session {
  id: string;
  title: string;
  model?: string;
  project?: string | null;
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

export interface MessageStats {
  eval_count: number; // jetons générés
  prompt_eval_count: number; // jetons du prompt
  tokens_per_sec: number | null; // vitesse de génération
}

export interface Message {
  id: string;
  session_id: string;
  role: "user" | "assistant";
  content: string;
  model?: string;
  meta?: {
    tools?: ToolCall[];
    stats?: MessageStats;
    thinking?: string;
    plan?: string[];
    engine?: string;
  } | null;
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
  think: boolean;
  code_model: string;
  plan_mode: boolean;
  self_review: boolean;
  rag_enabled: boolean;
  embed_model: string;
  skills_enabled: boolean;
  ponytail: boolean;
  keep_alive: string;
}

// ── Benchmark de modèles ─────────────────────────────────────────────────
export interface BenchDetail {
  task: string;
  score: number;
  detail: string;
}

export interface BenchResult {
  score: number;
  details: BenchDetail[];
  at: number;
}

export async function getBenchScores(): Promise<Record<string, BenchResult>> {
  const res = await fetch("/api/bench");
  return (await res.json()).scores;
}

/** Lance le benchmark d'un modèle en streamant la progression. */
export async function runBench(
  model: string,
  onProgress: (task: string, score: number | null, detail?: string) => void
): Promise<BenchResult | null> {
  let res: Response;
  try {
    res = await fetch("/api/bench", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model }),
    });
  } catch (err) {
    const raw = err instanceof Error ? err.message : "connexion impossible";
    throw new Error(
      /network error|failed to fetch|load failed/i.test(raw)
        ? "connexion au benchmark interrompue par le réseau ou le reverse proxy"
        : raw
    );
  }
  if (!res.ok) {
    throw await apiError(res, `benchmark refusé (${res.status})`);
  }
  if (!res.body) throw new Error("le serveur n'a pas renvoyé de progression");

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let final: BenchResult | null = null;

  const dispatch = (block: string) => {
    let event = "message";
    const dataLines: string[] = [];
    for (const line of block.replace(/\r\n/g, "\n").split("\n")) {
      if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) dataLines.push(line.slice(5).trimStart());
    }
    if (dataLines.length === 0) return;
    const payload = JSON.parse(dataLines.join("\n"));
    if (event === "task_start") onProgress(payload.task, null);
    else if (event === "task_done")
      onProgress(payload.task, payload.score, payload.detail);
    else if (event === "error")
      throw new Error(payload.message ?? "le benchmark a échoué");
    else if (event === "done")
      final = { score: payload.score, details: payload.details, at: Date.now() / 1000 };
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
    const raw = err instanceof Error ? err.message : "connexion interrompue";
    if (/network error|failed to fetch|load failed/i.test(raw)) {
      throw new Error("flux du benchmark interrompu par le reverse proxy");
    }
    throw err;
  }
  if (!final) throw new Error("le benchmark s'est interrompu avant le résultat");
  return final;
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

export async function listFiles(project?: string | null): Promise<FileNode[]> {
  const query = project ? `?project=${encodeURIComponent(project)}` : "";
  const res = await fetch(`/api/files${query}`);
  return (await res.json()).tree;
}

export async function fileContent(
  path: string,
  project?: string | null
): Promise<string> {
  const res = await fetch(
    `/api/files/content?path=${encodeURIComponent(path)}${projQuery(project)}`
  );
  if (!res.ok) return "";
  return (await res.json()).content;
}

export async function deleteFile(
  path: string,
  project?: string | null
): Promise<void> {
  const res = await fetch(
    `/api/files?path=${encodeURIComponent(path)}${projQuery(project)}`,
    { method: "DELETE" }
  );
  if (!res.ok) {
    throw await apiError(res, `suppression impossible (${res.status})`);
  }
}

export function downloadFile(path: string, project?: string | null): void {
  const link = document.createElement("a");
  link.href = `/api/files/download?path=${encodeURIComponent(path)}${projQuery(project)}`;
  link.download = path.split(/[\\/]/).pop() ?? "fichier";
  document.body.appendChild(link);
  link.click();
  link.remove();
}

export interface McpServer {
  id: string;
  label: string;
  description: string;
  url_param: boolean;
  env_params: string[];
  enabled: boolean;
  params: Record<string, string>;
  state: "inactive" | "connected" | "error";
  error: string | null;
  tools: number;
}

export async function listMcp(): Promise<McpServer[]> {
  const res = await fetch("/api/mcp");
  return (await res.json()).servers;
}

export async function updateMcp(
  id: string,
  enabled: boolean,
  params: Record<string, string>
): Promise<McpServer[]> {
  const res = await fetch(`/api/mcp/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ enabled, params }),
  });
  if (!res.ok) throw await apiError(res, "mise à jour MCP impossible");
  return (await res.json()).servers;
}

export async function testMcp(
  id: string
): Promise<{ ok: boolean; tools: string[]; error: string | null }> {
  const res = await fetch(`/api/mcp/${id}/test`, { method: "POST" });
  return res.json();
}

export async function listSessions(): Promise<Session[]> {
  const res = await fetch("/api/sessions");
  return (await res.json()).sessions;
}

export async function createSession(
  model?: string,
  project?: string | null
): Promise<Session> {
  const res = await fetch("/api/sessions", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      title: "Nouvelle session",
      model,
      project: project ?? null,
    }),
  });
  return res.json();
}

export async function listProjects(): Promise<{
  projects: { name: string; files: number }[];
  root_files: number;
}> {
  const res = await fetch("/api/projects");
  return res.json();
}

export async function createProject(name: string): Promise<void> {
  const res = await fetch("/api/projects", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) throw await apiError(res, "création du projet impossible");
}

export async function setSessionProject(
  id: string,
  project: string | null
): Promise<void> {
  const res = await fetch(`/api/sessions/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ project: project ?? "" }),
  });
  if (!res.ok) throw await apiError(res, "changement de projet impossible");
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

export async function renameSession(id: string, title: string): Promise<void> {
  const res = await fetch(`/api/sessions/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title }),
  });
  if (!res.ok) {
    throw await apiError(res, `renommage impossible (${res.status})`);
  }
}

/** Envoie un message et streame la réponse de l'agent via SSE. */
export async function streamChat(
  body: { session_id: string; content: string; model?: string; mode?: string },
  handlers: {
    onToken: (t: string) => void;
    onThinking: (t: string) => void;
    onToolCall: (call: ToolCall) => void;
    onToolResult: (call: ToolCall) => void;
    onToolConfirm: (command: string) => void;
    onStatus: (msg: string) => void;
    onNotice: (msg: string) => void;
    onPlan?: (steps: string[]) => void;
    onPlanStep?: (index: number) => void;
    onRevision?: (content: string) => void;
    onDone: (full: string) => void;
    onError: (msg: string) => void;
    onAbort?: () => void;
  },
  signal?: AbortSignal
): Promise<void> {
  let res: Response;
  try {
    res = await fetch("/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal,
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") {
      handlers.onAbort?.();
      return;
    }
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
      else if (event === "plan") handlers.onPlan?.(payload.steps);
      else if (event === "plan_step") handlers.onPlanStep?.(payload.index);
      else if (event === "revision") handlers.onRevision?.(payload.content);
      else if (event === "thinking") handlers.onThinking(payload.content);
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
      if (err instanceof DOMException && err.name === "AbortError") {
        handlers.onAbort?.();
        return;
      }
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
