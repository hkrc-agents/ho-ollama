// UI adapter only: the original Ollama core owns storage, TLS and resume.
export interface PullProgress {
  status: string;
  digest?: string;
  total?: number;
  completed?: number;
  error?: string;
  model?: string;
}

export async function downloadModel(
  model: string, signal: AbortSignal, progress: (event: PullProgress) => void,
) {
  const response = await fetch("/api/pull", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ model, stream: true }), signal,
  });
  return readOllamaProgress(response, signal, progress);
}

// Shared upstream NDJSON progress contract (pull/create), not a new model parser.
export async function readOllamaProgress(
  response: Response, signal: AbortSignal, progress: (event: PullProgress) => void,
  operation = "Download",
) {
  if (!response.ok) throw new Error(await response.text());
  if (!response.body) throw new Error(`${operation} response has no stream`);
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "", success = false;
  function consume(line: string) {
    if (!line.trim()) return;
    const event = JSON.parse(line) as PullProgress;
    if (event.error) throw new Error(event.error);
    if (typeof event.status !== "string") throw new Error("Invalid download progress");
    progress(event);
    if (event.status === "success") success = true;
  }
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) { buffer += decoder.decode(); consume(buffer); break; }
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split("\n");
      buffer = lines.pop() || "";
      for (const line of lines) consume(line);
    }
    if (signal.aborted) throw new DOMException(`${operation} cancelled`, "AbortError");
    if (!success) throw new Error(`${operation} ended before Ollama confirmed success`);
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}
