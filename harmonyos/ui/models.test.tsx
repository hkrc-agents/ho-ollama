import { act, create, type ReactTestRenderer } from "react-test-renderer";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ModelsPage, requireHarmonyModelsRoute } from "@/routes/models";

const mocks = vi.hoisted(() => ({
  refetch: vi.fn().mockResolvedValue(undefined),
  download: vi.fn().mockResolvedValue(undefined),
  import: vi.fn().mockResolvedValue("harmony-import-test:latest"),
  tasks: [{model:"pending:latest",status:"paused",digests:{one:{total:100,completed:50}}}],
}));
vi.mock("@tanstack/react-query", () => ({useQuery: () => ({data:{
  models:[{name:"ready:latest",size:100}],loaded:new Set(),downloads:mocks.tasks,
},refetch:mocks.refetch})}));
vi.mock("@tanstack/react-router", () => ({createFileRoute: () => (options: object) => options,
  redirect: (options: object) => ({redirect: options})}));
vi.mock("@/components/AppSidebar", () => ({AppSidebar: () => <nav />}));
vi.mock("@/components/layout/layout", () => ({SidebarLayout: ({children}: PropsWithChildren) => <div>{children}</div>}));
vi.mock("@/components/ui/button", () => ({Button: "button"}));
vi.mock("@/lib/model-download", () => ({downloadModel: mocks.download}));
vi.mock("@/lib/model-import", () => ({importGGUF: mocks.import}));
let tree: ReactTestRenderer;
beforeEach(() => { mocks.download.mockReset().mockResolvedValue(undefined); mocks.import.mockReset().mockResolvedValue("harmony-import-test:latest"); mocks.refetch.mockClear();
  mocks.tasks[0].status="paused"; vi.stubGlobal("fetch", vi.fn(async () => new Response(null,{status:204}))); });
afterEach(async () => { if(tree) await act(async () => tree.unmount()); vi.unstubAllGlobals(); });
async function render() { await act(async () => {tree=create(<ModelsPage />);}); }
describe("persistent incomplete downloads", () => {
  it.each(["windows", "darwin", undefined])("rejects the HarmonyOS route on %s before fetching", platform => {
    vi.stubGlobal("window", {OLLAMA_PLATFORM: platform});
    expect(() => requireHarmonyModelsRoute()).toThrow();
    expect(fetch).not.toHaveBeenCalled();
  });
  it("allows the route only on HarmonyOS", () => {
    vi.stubGlobal("window", {OLLAMA_PLATFORM: "harmonyos"});
    expect(() => requireHarmonyModelsRoute()).not.toThrow();
    expect(fetch).not.toHaveBeenCalled();
  });
  it("selects a single GGUF and delegates to the import adapter", async () => {
    await render();
    const source = {name:"test.gguf",size:32};
    const target = {files:[source],value:"chosen"};
    await act(async () => tree.root.findByProps({"aria-label":"GGUF file to import"}).props.onChange({target}));
    expect(target.value).toBe("");
    expect(mocks.import).toHaveBeenCalledWith(source,expect.any(AbortSignal),expect.any(Function));
    expect(mocks.refetch).toHaveBeenCalled();
    expect(fetch).not.toHaveBeenCalled();
  });
  it("offers Resume instead of Load for an incomplete model", async () => {
    await render();
    expect(tree.root.findAllByProps({"aria-label":"Load pending:latest"})).toHaveLength(0);
    expect(tree.root.findAllByProps({"aria-label":"Load ready:latest"})).toHaveLength(1);
    await act(async () => tree.root.findByProps({"aria-label":"Resume download pending:latest"}).props.onClick());
    expect(mocks.download).toHaveBeenCalledWith("pending:latest",expect.any(AbortSignal),expect.any(Function));
    expect(fetch).not.toHaveBeenCalled();
    expect(mocks.refetch).toHaveBeenCalled();
  });
  it("requires confirmation and only forgets the record", async () => {
    await render();
    await act(async () => tree.root.findByProps({"aria-label":"Forget download pending:latest"}).props.onClick());
    expect(fetch).not.toHaveBeenCalled();
    const confirm=tree.root.findAllByType("button").find(button=>button.children.includes("Confirm forget record"));
    expect(confirm).toBeDefined();
    await act(async () => confirm!.props.onClick());
    expect(fetch).toHaveBeenCalledWith("/api/harmony/downloads",expect.objectContaining({method:"DELETE",body:'{"model":"pending:latest"}'}));
  });
  it("disables a second request for an already active task", async () => {
    mocks.tasks[0].status="downloading"; await render();
    expect(tree.root.findByProps({"aria-label":"Resume download pending:latest"}).props.disabled).toBe(true);
    expect(tree.root.findByProps({"aria-label":"Forget download pending:latest"}).props.disabled).toBe(true);
  });
  it("hides completed task records from incomplete rows", async () => {
    mocks.tasks[0].status="completed"; await render();
    expect(tree.root.findAllByProps({"aria-label":"Incomplete download pending:latest"})).toHaveLength(0);
  });
});
