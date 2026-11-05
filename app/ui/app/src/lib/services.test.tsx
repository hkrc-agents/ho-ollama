import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { HarmonyServices } from "./services";
vi.mock("@/components/ui/button", () => ({ Button: (p: object) => <button {...p} /> }));
let tree: ReactTestRenderer | undefined;
const state = { ollama: { state: "ready" }, llama: { state: "stopped", config: {
  model: "", port: 18082, threads: 2, context: 1024, memory_mib: 1536,
} }, models: [{ name: "example:tag", path: "/authorized/model.gguf" }] };
beforeEach(() => {
  vi.useFakeTimers(); vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal("window", { setInterval, clearInterval });
  vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, json: async () => state })));
});
afterEach(async () => { if (tree) await act(async () => tree!.unmount()); tree = undefined; vi.unstubAllGlobals(); vi.useRealTimers(); });
async function mount() { await act(async () => { tree = create(<HarmonyServices />); }); }
it("keeps advanced service controls collapsed on initial Settings load", async () => {
  await mount(); expect(tree!.root.findByType("details").props.open).toBeUndefined();
});
it("renders separate core and standalone lifecycle controls", async () => {
  await mount(); expect(tree!.root.findByProps({ "aria-label": "ollama service status" }).children).toEqual(["ready"]);
  expect(tree!.root.findByProps({ "aria-label": "llama service status" }).children).toEqual(["stopped"]);
  expect(tree!.root.findByProps({ "aria-label": "Standalone GGUF path" }).props.value).toBe("/authorized/model.gguf");
});
it("starts with selected configuration, not a fixed model or predict limit", async () => {
  await mount(); await act(async () => tree!.root.findByProps({ "aria-label": "start llama service" }).props.onClick());
  const requests = vi.mocked(fetch).mock.calls.filter(c => c[1]?.method === "POST");
  const body = JSON.parse(requests[0][1]!.body as string);
  expect(body).toMatchObject({ service: "llama", action: "start", config: { model: "/authorized/model.gguf", threads: 2 } });
  expect(JSON.stringify(body)).not.toContain("predict");
});
it("reports service request errors", async () => {
  await mount(); vi.mocked(fetch).mockResolvedValueOnce({ ok: false, text: async () => "port occupied" } as Response);
  await act(async () => tree!.root.findByProps({ "aria-label": "start llama service" }).props.onClick());
  expect(tree!.root.findByProps({ role: "alert" }).children.join("")).toContain("port occupied");
});
it("does not overwrite edited configuration during polling", async () => {
  await mount(); await act(async () => tree!.root.findByProps({ "aria-label": "Standalone Port" }).props.onChange({ target: { value: "18091" } }));
  await act(async () => vi.advanceTimersByTime(2000));
  expect(tree!.root.findByProps({ "aria-label": "Standalone Port" }).props.value).toBe(18091);
});
it("clears its polling timer on unmount", async () => {
  await mount(); await act(async () => tree!.unmount()); tree = undefined; expect(vi.getTimerCount()).toBe(0);
});
