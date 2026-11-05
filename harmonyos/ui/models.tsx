import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { createFileRoute, redirect } from "@tanstack/react-router";
import { AppSidebar } from "@/components/AppSidebar";
import { SidebarLayout } from "@/components/layout/layout";
import { Button } from "@/components/ui/button";
import { downloadModel } from "@/lib/model-download";
import { importGGUF } from "@/lib/model-import";
import { HarmonyModelExport } from "@/lib/model-export";

interface ModelEntry {
  name: string;
  size: number;
  details?: { parameter_size?: string; quantization_level?: string };
}
interface DownloadEntry {
  model: string;
  status: "downloading" | "paused" | "failed" | "completed";
  digests: Record<string, { total: number; completed: number }>;
  error?: string;
}
interface ImportEntry { model: string; digest: string; size: number; status: string }
async function call(path: string, data?: object, method?: string) {
  const result = await fetch(path, { method: method || (data ? "POST" : "GET"),
    headers: { "Content-Type": "application/json" },
    body: data ? JSON.stringify(data) : undefined });
  if (!result.ok) throw new Error(await result.text());
  const text = await result.text();
  return text ? JSON.parse(text) : {};
}

export function requireHarmonyModelsRoute() {
  if (typeof window === "undefined" || window.OLLAMA_PLATFORM !== "harmonyos") {
    throw redirect({ to: "/" });
  }
}
export const Route = createFileRoute("/models")({ beforeLoad: requireHarmonyModelsRoute, component: ModelsPage });
export function ModelsPage() {
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [confirm, setConfirm] = useState("");
  const [modelName, setModelName] = useState("");
  const [downloadStatus, setDownloadStatus] = useState("");
  const [details, setDetails] = useState<{ name: string; text: string } | null>(null);
  const [forgetConfirm, setForgetConfirm] = useState("");
  const [importStatus, setImportStatus] = useState("");
  const download = useRef<AbortController | null>(null);
  useEffect(() => () => download.current?.abort(), []);
  const query = useQuery({ queryKey: ["harmony-model-management"], queryFn: async () => {
    const [tags, running, jobs, imports] = await Promise.all([call("/api/tags"), call("/api/ps"), call("/api/harmony/downloads"), call("/api/harmony/imports")]);
    return { models: (tags.models || []) as ModelEntry[], loaded: new Set<string>(
      (running.models || []).map((m: ModelEntry) => m.name)),
      downloads: (jobs.downloads || []) as DownloadEntry[], imports: (imports.imports || []) as ImportEntry[],
      importIssues: (imports.issues || []) as {file: string; error: string}[] };
  }, refetchInterval: busy ? false : 5000 });
  async function action(name: string, operation: "load" | "unload" | "delete") {
    setBusy(name); setError("");
    try {
      if (operation === "delete") await call("/api/delete", { model: name }, "DELETE");
      else await call("/api/generate", { model: name, prompt: "", stream: false,
        keep_alive: operation === "load" ? "5m" : 0 });
      setConfirm("");
      await query.refetch();
    } catch (e) { setError(String(e)); }
    finally { setBusy(""); }
  }
  async function pull(requested?: string) {
    const name = (requested || modelName).trim();
    if (!name || busy) return;
    const controller = new AbortController();
    download.current = controller;
    setModelName(name);
    setBusy("download:" + name); setError(""); setDownloadStatus("Starting download…");
    try {
      await downloadModel(name, controller.signal, event => {
        const bytes = typeof event.total === "number" && event.total > 0 && typeof event.completed === "number"
          ? ` · ${(event.completed / 1024 / 1024).toFixed(1)} / ${(event.total / 1024 / 1024).toFixed(1)} MiB` : "";
        setDownloadStatus(event.status + bytes);
      });
      setDownloadStatus("Download complete");
    } catch (e) {
      if (controller.signal.aborted) setDownloadStatus("Download cancelled; downloaded data is retained by Ollama for retry.");
      else { setError(String(e)); setDownloadStatus("Download failed"); }
    } finally {
      download.current = null; setBusy(""); await query.refetch();
    }
  }
  async function forget(name: string) {
    setBusy("forget:" + name); setError("");
    try {
      await call("/api/harmony/downloads", { model: name }, "DELETE");
      setForgetConfirm(""); await query.refetch();
    } catch (e) { setError(String(e)); }
    finally { setBusy(""); }
  }
  async function importFile(file: File) {
    if (busy) return;
    const controller = new AbortController(); download.current = controller;
    setBusy("import"); setError(""); setImportStatus("Copying selected GGUF to private staging…");
    try {
      const name = await importGGUF(file, controller.signal, event => setImportStatus(
        `${event.status}${event.model ? " · " + event.model : ""}`));
      setImportStatus(`Imported ${name}. The source file is unchanged.`);
    } catch (e) {
      setError(String(e));
      setImportStatus("Import not confirmed. Check import records and the model list before retrying; cancellation does not roll back a model already created by Ollama.");
    } finally { download.current = null; setBusy(""); await query.refetch(); }
  }
  async function reconcileImport(name: string) {
    setBusy("reconcile"); setError("");
    try { await call("/api/harmony/imports/reconcile", {model: name}); await query.refetch(); }
    catch (e) { setError(String(e)); } finally { setBusy(""); }
  }
  async function archiveImport(file: string) {
    setBusy("archive"); setError("");
    try { await call("/api/harmony/imports/archive", {file, confirm: true}); await query.refetch(); }
    catch (e) { setError(String(e)); } finally { setBusy(""); }
  }
  async function show(name: string) {
    setBusy(name); setError("");
    try {
      const result = await call("/api/show", { model: name });
      setDetails({ name, text: JSON.stringify({ details: result.details, capabilities: result.capabilities,
        model_info: result.model_info, license: result.license }, null, 2) });
    } catch (e) { setError(String(e)); }
    finally { setBusy(""); }
  }
  return <SidebarLayout title="Models" sidebar={<AppSidebar current="models" />}>
    <div className="flex-1 overflow-y-auto p-6 space-y-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-neutral-500">Downloaded models managed by Ollama</p>
        <Button color="white" onClick={() => void query.refetch()} disabled={!!busy}>Refresh</Button>
      </div>
      <form className="space-y-2" onSubmit={event => { event.preventDefault(); void pull(); }}>
        <label htmlFor="download-model" className="text-sm dark:text-white">Download an Ollama model</label>
        <div className="flex gap-2">
          <input id="download-model" aria-label="Model to download" value={modelName}
            disabled={!!busy} onChange={event => setModelName(event.target.value)}
            placeholder="Model name:tag" className="border rounded-lg px-3 py-2 dark:text-white" />
          <Button color="white" type="submit" disabled={!!busy || !modelName.trim()}>Download</Button>
          {download.current && busy !== "import" && <Button color="white" type="button" onClick={() => download.current?.abort()}>Cancel download</Button>}
        </div>
        <p className="text-xs text-neutral-500">Downloads use disk space. Choose a model that fits this device; no model is downloaded automatically.</p>
        {downloadStatus && <p role="status" className="text-sm dark:text-white">{downloadStatus}</p>}
      </form>
      <section aria-label="Import GGUF" className="space-y-2">
        <label htmlFor="import-gguf" className="text-sm dark:text-white">Import a local GGUF using Ollama</label>
        <input id="import-gguf" type="file" accept=".gguf" disabled={!!busy} aria-label="GGUF file to import"
          className="block text-sm dark:text-white" onChange={event => {
            const file = event.target.files?.[0]; event.target.value = "";
            if (file) void importFile(file);
          }} />
        <p className="text-xs text-neutral-500">Single-file models only, up to 4 GiB. Uses a new generated name; does not overwrite existing names. Requires space for private staging and the Ollama copy. No automatic loading or re-quantization.</p>
        {busy === "import" && <Button color="white" onClick={() => download.current?.abort()}>Cancel import</Button>}
        {importStatus && <p role="status" className="text-sm dark:text-white">{importStatus}</p>}
      </section>
      {query.data?.importIssues?.map(issue => <section key={issue.file} className="space-y-2">
        <p role="alert" className="text-sm text-red-600">Import record {issue.file}: {issue.error}</p>
        {/^[0-9a-f]{32}\.(json|complete)$/.test(issue.file) && <Button color="white" disabled={!!busy}
          onClick={() => void archiveImport(issue.file)}>Archive damaged record (keep model data)</Button>}
      </section>)}
      {query.data?.imports?.filter(record => record.status !== "completed" && record.status !== "recovered").map(record => <section key={record.model}
        aria-label={"Unconfirmed import " + record.model} className="rounded-xl border p-4 space-y-2">
        <h2 className="dark:text-white">{record.model}</h2>
        <p className="text-sm text-neutral-500">Import result unconfirmed. This is not proof of rollback. Refresh and inspect this name in the model list before importing again. Shared blobs are not automatically removed.</p>
        <Button color="white" disabled={!!busy} onClick={() => void reconcileImport(record.model)}>Check imported model</Button>
      </section>)}
      {details && <section aria-label={"Details of " + details.name} className="space-y-2">
        <div className="flex items-center justify-between gap-2">
          <h2 className="dark:text-white">{details.name}</h2>
          <Button color="white" onClick={() => setDetails(null)}>Close details</Button>
        </div>
        <pre className="max-h-96 overflow-auto text-xs whitespace-pre-wrap break-all dark:text-white">{details.text}</pre>
      </section>}
      {(error || query.error) && <p role="alert" className="text-red-600">{error || String(query.error)}</p>}
      {query.isLoading && <p>Loading models…</p>}
      {query.data?.downloads.filter(task => task.status !== "completed").map(task => {
        const values = Object.values(task.digests);
        const completed = values.reduce((sum, value) => sum + value.completed, 0);
        const total = values.reduce((sum, value) => sum + value.total, 0);
        return <section key={"download:" + task.model} aria-label={"Incomplete download " + task.model}
          className="rounded-xl border border-neutral-200 dark:border-neutral-700 p-4 space-y-3">
          <h2 className="text-base font-medium dark:text-white">{task.model}</h2>
          <p className="text-sm dark:text-white">{task.status === "downloading" ? "Downloading" : task.status === "failed" ? "Download interrupted" : "Download paused"}</p>
          <p className="text-xs text-neutral-500">Last reported: {(completed / 1024 / 1024).toFixed(1)} / {(total / 1024 / 1024).toFixed(1)} MiB of known blobs. Shared data is not exclusive disk usage.</p>
          {task.error && <p className="text-sm text-red-600">{task.error}</p>}
          <div className="flex gap-2">
            <Button color="white" disabled={!!busy || task.status === "downloading"}
              aria-label={"Resume download " + task.model} onClick={() => void pull(task.model)}>Resume download</Button>
            <Button color="white" disabled={!!busy || task.status === "downloading"}
              aria-label={"Forget download " + task.model} onClick={() => setForgetConfirm(task.model)}>Forget record</Button>
          </div>
          {forgetConfirm === task.model && <div role="dialog" aria-label="Forget download record" className="space-y-2">
            <p className="text-sm dark:text-white">Remove this record? Cached chunks may remain. No model data will be deleted.</p>
            <Button color="white" disabled={!!busy} onClick={() => void forget(task.model)}>Confirm forget record</Button>
            <Button color="white" disabled={!!busy} onClick={() => setForgetConfirm("")}>Keep record</Button>
          </div>}
        </section>;
      })}
      {query.data?.models.length === 0 && <p>No downloaded models. Download one above or use the original Ollama CLI through HDC to import a model.</p>}
      {query.data?.models.map(model => <section key={model.name}
        className="rounded-xl border border-neutral-200 dark:border-neutral-700 p-4 space-y-3">
        <h2 className="text-base font-medium dark:text-white">{model.name}</h2>
        <p className="text-sm text-neutral-500">
          {(model.size / 1024 / 1024).toFixed(1)} MiB · {model.details?.parameter_size || "Unknown parameters"} · {model.details?.quantization_level || "Unknown quantization"}
        </p>
        <p className="text-sm dark:text-white">{busy === model.name ? "Working…" : query.data.loaded.has(model.name) ? "Loaded" : "Not loaded"}</p>
        <div className="flex gap-2 flex-wrap">
          <Button color="white" disabled={!!busy} aria-label={"Details " + model.name}
            onClick={() => void show(model.name)}>Details</Button>
          <Button color="white" disabled={!!busy || query.data.loaded.has(model.name)}
            aria-label={"Load " + model.name} onClick={() => void action(model.name, "load")}>Load</Button>
          <Button color="white" disabled={!!busy || !query.data.loaded.has(model.name)}
            aria-label={"Unload " + model.name} onClick={() => void action(model.name, "unload")}>Unload</Button>
          <Button color="white" disabled={!!busy} aria-label={"Delete " + model.name}
            onClick={() => setConfirm(model.name)}>Delete</Button>
        </div>
        <HarmonyModelExport model={model.name} disabled={!!busy} />
        {confirm === model.name && <div role="dialog" aria-label={"Confirm deletion of " + model.name}
          className="rounded-lg bg-neutral-100 dark:bg-neutral-800 p-3 space-y-2">
          <p className="text-sm dark:text-white">Delete {model.name}? Other model names are not removed.</p>
          <Button color="white" disabled={!!busy} onClick={() => void action(model.name, "delete")}>Confirm delete</Button>
          <Button color="white" disabled={!!busy} onClick={() => setConfirm("")}>Cancel</Button>
        </div>}
      </section>)}
    </div>
  </SidebarLayout>;
}
