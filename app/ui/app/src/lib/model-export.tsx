import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";

interface TransferState { model: string; status: string; phase: string; error: string; bytes: number; total: number }

export function HarmonyModelExport({ model, disabled }: { model: string; disabled: boolean }) {
  const [state, setState] = useState<TransferState | null>(null);
  const supported = typeof window !== "undefined" && window.OLLAMA_PLATFORM === "harmonyos";
  useEffect(() => {
    if (!supported || typeof window === "undefined") return;
    const listener = (event: Event) => {
      const state = (event as CustomEvent<TransferState>).detail;
      if (state && state.model === model) setState(state);
    };
    window.addEventListener("harmony-model-transfer", listener);
    if (supported) window.location.href = "harmony-ollama://model-transfer-status";
    return () => window.removeEventListener("harmony-model-transfer", listener);
  }, [model, supported]);
  const running = state?.status === "running";
  return <div className="space-y-2">
    <Button color="white" aria-label={"Export GGUF " + model} disabled={!supported || disabled || running}
      onClick={() => { window.location.href = "harmony-ollama://export-gguf?model=" + encodeURIComponent(model); }}>Export GGUF</Button>
    <p className="text-xs text-neutral-500">Single-file weights only, not a full Ollama backup. Choose a new/empty output file. Do not approve replacement of an existing file in the system dialog.</p>
    {running && state.phase !== "publishing" && <Button color="white" onClick={() => {
      window.location.href = "harmony-ollama://cancel-model-export";
    }}>Cancel export</Button>}
    {state && <p role="status" className="text-sm dark:text-white">Export {state.status}: {state.phase} · {(state.bytes / 1048576).toFixed(1)} / {(state.total / 1048576).toFixed(1)} MiB{state.error ? " · " + state.error : ""}</p>}
  </div>;
}
