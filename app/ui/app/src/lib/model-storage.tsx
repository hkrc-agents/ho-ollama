import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";

interface Selection { id: string; path: string; persisted: boolean; activated: boolean; error?: string }
interface Migration { id: string; state: string; mode?: string; old_root: string; new_root: string; copied: number; bytes: number; error?: string }
interface Storage { root: string; configured_root?: string; default_root?: string; mode?: "private" | "custom" | "unknown"; error: string; authorization_error?: string; blocked: string; migration: Migration | null; running: boolean }
async function request(path: string, body?: object): Promise<Storage> {
  const response = await fetch("/api/harmony/storage/" + path, {
    method: body ? "POST" : "GET", headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined,
  });
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}

export function HarmonyModelStorage() {
  const [state, setState] = useState<Storage | null>(null);
  const [selection, setSelection] = useState<Selection | null>(null);
  const [error, setError] = useState("");
  const [confirm, setConfirm] = useState<"custom" | "private" | null>(null);
  const [busy, setBusy] = useState(false);
  const refresh = async () => {
    try {
      setState(await request("status"));
      const response = await fetch("/api/harmony/storage/selection");
      if (response.ok) setSelection(await response.json()); else setSelection(null);
    } catch (e) { setError(String(e)); }
  };
  useEffect(() => { void refresh(); const interval = setInterval(() => void refresh(), 3000); return () => clearInterval(interval); }, []);
  async function act(name: string, data: object = {}) {
    setBusy(true); setError("");
    try { await request(name, data); setConfirm(null); await refresh(); }
    catch (e) { setError(String(e)); } finally { setBusy(false); }
  }
  const authorized = selection?.persisted && selection.activated && !selection.error;
  return <details aria-label="Model storage and migration" className="rounded-xl bg-white dark:bg-neutral-800 p-4 space-y-4">
    <summary className="cursor-pointer font-medium dark:text-white">Model storage and migration</summary>
    <p className="text-sm dark:text-white">Storage mode: {state?.mode === "private" ? "Private app folder (default)" : state?.mode === "custom" ? "Custom folder" : "unavailable"}</p>
    <p className="text-xs text-neutral-500">The private app folder works without folder authorization. A custom folder requires system authorization. Your saved library is restored on startup; choosing a folder alone does not switch libraries.</p>
    <p className="text-sm dark:text-white break-all">Active library: {state?.root || "unavailable"}</p>
    {state?.configured_root && state.configured_root !== state.root && <p className="text-sm dark:text-white break-all">Saved library: {state.configured_root}</p>}
    {(state?.error || state?.blocked) && <p role="alert" className="text-sm text-red-600">{state.error || state.blocked}</p>}
    {state?.authorization_error && <p role="alert" className="text-sm text-red-600">{state.authorization_error}</p>}
    <Button color="white" disabled={busy || state?.running} onClick={() => {
      setConfirm(null);
      window.location.href = "harmony-ollama://model-directory";
    }}>Choose custom storage folder</Button>
    <Button color="white" disabled={!state || state.mode !== "custom" || busy || state.running || !!state.blocked} onClick={() => setConfirm("private")}>Use default private folder</Button>
    <p className="text-sm dark:text-white break-all">Selected destination parent: {selection?.path || "none"}</p>
    <p className="text-xs text-neutral-500">Migration creates a dedicated model-library subfolder inside the selected folder, verifies the copy, then switches libraries. The old library is retained.</p>
    <p className="text-xs text-neutral-500">{authorized ? "Native persistent authorization confirmed; actual file operations will be checked again before migration." :
      "Persistent directory authorization is not confirmed. Your selection is saved, but migration is disabled until authorization succeeds. Permission persistence across app and system restarts must be verified on a physical device (E6)."}</p>
    <Button color="white" disabled={!authorized || !state || busy || state.running || !!state.blocked || !!state.error} onClick={() => setConfirm("custom")}>Migrate model library</Button>
    {confirm && <div role="dialog" aria-label="Confirm model migration" className="space-y-2">
      <p className="text-sm dark:text-white">{confirm === "private" ?
        "Stop both inference services and switch to the retained private library. Models in the custom folder remain there; they are not copied or merged. A running standalone server can restart only if its model exists in the private library; otherwise the switch rolls back. Export any models you need before switching." :
        "Stop both inference services, clear in-memory sessions, copy and verify the library into a new dedicated folder, then switch. The old library is retained. Additional disk space is required; do not edit either library with external tools during migration."}</p>
      {confirm === "private" && <p className="text-sm break-all dark:text-white">Default library: {state?.default_root}</p>}
      <Button color="white" disabled={busy || !!state?.running || !!state?.blocked || (confirm === "custom" && (!authorized || !!state?.error))} onClick={() => void act("migrate", confirm === "private" ? {mode: "private", confirm: true} : {selection_id: selection?.id, confirm: true})}>Confirm migration</Button>
      <Button color="white" disabled={busy} onClick={() => setConfirm(null)}>Keep current library</Button>
    </div>}
    {state?.migration && <div className="space-y-2 text-sm dark:text-white">
      <p role="status">Migration {state.migration.state} · {state.migration.copied} files · {(state.migration.bytes / 1048576).toFixed(1)} MiB</p>
      <p className="break-all">{state.migration.old_root} → {state.migration.new_root}</p>
      {state.migration.error && <p className="text-red-600">{state.migration.error}</p>}
      {state.running && <Button color="white" disabled={busy || ["switching", "starting"].includes(state.migration.state)} onClick={() => void act("cancel")}>Cancel migration</Button>}
      {!state.running && state.migration.state === "rolled_back" && state.migration.mode !== "private" && <Button color="white" disabled={busy || !authorized} onClick={() => void act("discard", {id: state.migration?.id, confirm: true})}>Discard verified partial copy</Button>}
    </div>}
    <Button color="white" disabled={busy || state?.running} onClick={() => void act("recover")}>Recover interrupted migration</Button>
    {error && <p role="alert" className="text-sm text-red-600">{error}</p>}
  </details>;
}
