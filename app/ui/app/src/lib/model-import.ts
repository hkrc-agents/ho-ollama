import { readOllamaProgress, type PullProgress } from "./model-download";

// File goes to fetch unchanged; no arrayBuffer(), base64 or attachment bridge.
// The private ingress hashes bounded chunks and calls official CreateBlob/Create.
export async function importGGUF(file: File, signal: AbortSignal,
  progress: (event: PullProgress) => void): Promise<string> {
  if (!file.name.toLowerCase().endsWith(".gguf") || file.size < 24 || file.size > 4 * 1024 ** 3) {
    throw new Error("Choose one GGUF file (24 bytes to 4 GiB). Split/projector sets are not supported by this import form.");
  }
  const response = await fetch("/api/harmony/gguf", {
    method: "POST", headers: { "Content-Type": "application/octet-stream", "X-GGUF-Size": String(file.size) },
    body: file, signal,
  });
  let model = "";
  await readOllamaProgress(response, signal, event => {
    if (event.model) model = event.model;
    progress(event);
  }, "Import");
  if (!model) throw new Error("Ollama did not return an imported model name");
  return model;
}
