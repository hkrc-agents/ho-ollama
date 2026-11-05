import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";

type Config = { model: string; port: number; threads: number; context: number; memory_mib: number };
type State = { state: string; error?: string; config?: Config; identity?: { pid: number; start_ticks: string } };
type Services = { ollama: State; llama: State; models: { name: string; path: string }[] };
const initial: Config = { model: "", port: 18082, threads: 2, context: 1024, memory_mib: 1536 };
async function serviceRequest(path: string, data?: object) {
  const response = await fetch("/api/harmony/services" + path, {
    method: data ? "POST" : "GET", headers: { "Content-Type": "application/json" },
    body: data ? JSON.stringify(data) : undefined,
  });
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}
export function HarmonyServices() {
  const [services, setServices] = useState<Services | null>(null);
  const [config, setConfig] = useState<Config>(initial);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [logs, setLogs] = useState<string | null>(null);
  const initialized = useRef(false);
  useEffect(() => {
    let disposed = false;
    async function refresh() {
      try {
        const result: Services = await serviceRequest("");
        if (disposed) return;
        setServices(result);
        if (!initialized.current) {
          initialized.current = true;
          const saved = result.llama.config || initial;
          setConfig({ ...saved, model: saved.model || result.models[0]?.path || "" });
        }
      } catch (e) { if (!disposed) setError(String(e)); }
    }
    void refresh();
    const timer = window.setInterval(() => void refresh(), 2000);
    return () => { disposed = true; window.clearInterval(timer); };
  }, []);
  async function action(service: "ollama" | "llama", action: "start" | "stop" | "restart") {
    if (busy) return;
    setBusy(service + ":" + action); setError("");
    try {
      await serviceRequest("", { service, action, config });
      setServices(await serviceRequest(""));
    } catch (e) { setError(String(e)); }
    finally { setBusy(""); }
  }
  return <details aria-label="CPU inference services" className="rounded-xl bg-white dark:bg-neutral-800 p-4 space-y-4">
    <summary className="cursor-pointer font-medium dark:text-white">CPU inference services</summary>
    <p className="text-sm text-neutral-500">Public inference APIs use loopback TCP without additional client authentication. No NPU or OMC engine is enabled.</p>
    {(["ollama", "llama"] as const).map(service => <div key={service} className="space-y-2">
      <div className="flex items-center justify-between gap-4 dark:text-white">
        <span>{service === "ollama" ? "Ollama service" : "Independent llama-server"}</span>
        <span role="status" aria-label={service + " service status"}>{services?.[service].state || "checking"}</span>
      </div>
      {services?.[service].error && <p className="text-sm text-red-600">{services[service].error}</p>}
      <div className="flex flex-wrap gap-2">
        {(["start", "stop", "restart"] as const).map(verb => <Button key={verb} color="white"
          aria-label={verb + " " + service + " service"}
          disabled={!!busy || !services || (service === "llama" && verb !== "stop" && !config.model)}
          onClick={() => void action(service, verb)}>{verb[0].toUpperCase() + verb.slice(1)}</Button>)}
      </div>
    </div>)}
    <fieldset disabled={!!busy || services?.llama.state === "ready" || services?.llama.state === "starting"} className="space-y-2 text-sm dark:text-white">
      <legend className="font-medium">Independent llama-server configuration</legend>
      <label className="block">Installed GGUF model
        <select aria-label="Standalone installed model" className="block w-full border rounded p-2 dark:bg-neutral-800"
          value={config.model} onChange={e => setConfig({ ...config, model: e.target.value })}>
          <option value="">Choose an installed model</option>
          {services?.models.map((model, index) => <option key={model.path + index} value={model.path}>{model.name}</option>)}
        </select>
      </label>
      <label className="block">Authorized GGUF path
        <input aria-label="Standalone GGUF path" className="block w-full border rounded p-2 dark:bg-neutral-800"
          value={config.model} onChange={e => setConfig({ ...config, model: e.target.value })} />
      </label>
      <div className="flex flex-wrap gap-3">
        {([ ["port", "Port", 1024, 65535], ["threads", "CPU threads", 1, 64],
          ["context", "Context tokens", 128, 32768] ] as const).map(([key, label, min, max]) =>
          <label key={key}>{label}<input type="number" aria-label={"Standalone " + label} min={min} max={max}
            className="block w-32 border rounded p-2 dark:bg-neutral-800" value={config[key]}
            onChange={e => setConfig({ ...config, [key]: Number(e.target.value) })} /></label>)}
      </div>
      <p className="text-neutral-500">Stop before changing configuration. Startup validates model access and saves the configuration. No model starts automatically. Large models/contexts require sufficient device RAM.</p>
    </fieldset>
    <Button color="white" disabled={!!busy} onClick={async () => {
      try { setLogs((await serviceRequest("/logs")).log); } catch (e) { setError(String(e)); }
    }}>View llama-server logs</Button>
    {logs !== null && <div className="space-y-2"><pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all text-xs dark:text-white">{logs}</pre>
      <Button color="white" onClick={() => setLogs(null)}>Close server logs</Button></div>}
    {busy && <p role="status" className="text-sm text-neutral-500">{busy}…</p>}
    {error && <p role="alert" className="text-red-600">{error}</p>}
  </details>;
}
