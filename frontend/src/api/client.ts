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
