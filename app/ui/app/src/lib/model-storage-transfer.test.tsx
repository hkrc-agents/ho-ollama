import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { HarmonyModelStorage } from "./model-storage";
import { HarmonyModelExport } from "./model-export";
vi.mock("@/components/ui/button", () => ({ Button: "button" }));
let tree: ReactTestRenderer | undefined;
let listener: ((event: Event) => void) | undefined;
const state = {root:"/private/models",configured_root:"/private/models",mode:"private",error:"",blocked:"",running:false,migration:null};
let selection = {id:"selection",path:"/chosen",persisted:false,activated:false};
beforeEach(() => {
  vi.useFakeTimers(); vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal("window", {OLLAMA_PLATFORM:"harmonyos",location:{href:""},
    addEventListener:vi.fn((_name, callback) => { listener=callback; }),removeEventListener:vi.fn()});
  selection={id:"selection",path:"/chosen",persisted:false,activated:false};
  Object.assign(state,{root:"/private/models",configured_root:"/private/models",mode:"private",error:""});
  vi.stubGlobal("fetch", vi.fn(async (path: string) => ({ok:true,json:async()=>path.endsWith("selection")?selection:state})));
});
afterEach(async () => {if(tree) await act(async()=>tree!.unmount());tree=undefined;listener=undefined;vi.unstubAllGlobals();vi.useRealTimers();});
async function mount(node: React.ReactNode) {await act(async()=>{tree=create(node);});}
function button(label:string) {return tree!.root.findAllByType("button").find(b=>b.children.includes(label))!;}
it("aligns storage with the other settings cards", async()=>{
  await mount(<HarmonyModelStorage/>);
  const card=tree!.root.findByType("details");
  expect(card.props["aria-label"]).toBe("Model storage and migration");
  expect(card.props.className.split(/\s+/)).toEqual([
    "rounded-xl","bg-white","dark:bg-neutral-800","p-4","space-y-4",
  ]);
});
it("does not permit migration without a real persistent selection", async()=>{
  await mount(<HarmonyModelStorage/>);
  expect(button("Migrate model library").props.disabled).toBe(true);
  expect(tree!.root.findByType("details").props.open).toBeUndefined();
  await act(async()=>button("Choose custom storage folder").props.onClick());
  expect(window.location.href).toBe("harmony-ollama://model-directory");
});
it("shows the default private storage mode without requesting folder permission",async()=>{
  await mount(<HarmonyModelStorage/>);
  expect(JSON.stringify(tree!.toJSON())).toContain("Private app folder (default)");
  expect(window.location.href).toBe("");
});
it("keeps an unavailable custom library visible instead of claiming a private fallback",async()=>{
  Object.assign(state,{root:"",configured_root:"/chosen/ollama-models-id",mode:"custom",error:"selected model root unavailable; no fallback"});
  await mount(<HarmonyModelStorage/>);
  const output=JSON.stringify(tree!.toJSON());
  expect(output).toContain("Custom folder");
  expect(output).toContain("Saved library: ");
  expect(output).toContain("/chosen/ollama-models-id");
  expect(output).not.toContain("Private app folder (default)");
  expect(button("Migrate model library").props.disabled).toBe(true);
  expect(button("Use default private folder").props.disabled).toBe(false);
});
it("returns to the retained private library only after explicit confirmation",async()=>{
  Object.assign(state,{root:"/chosen/library",configured_root:"/chosen/library",mode:"custom"});
  await mount(<HarmonyModelStorage/>);
  await act(async()=>button("Use default private folder").props.onClick());
  expect(JSON.stringify(tree!.toJSON())).toContain("not copied or merged");
  expect(fetch).not.toHaveBeenCalledWith("/api/harmony/storage/migrate",expect.anything());
  await act(async()=>button("Confirm migration").props.onClick());
  expect(fetch).toHaveBeenCalledWith("/api/harmony/storage/migrate",expect.objectContaining({body:'{"mode":"private","confirm":true}'}));
});
it("disables return to private when already using the default library",async()=>{
  await mount(<HarmonyModelStorage/>);
  expect(button("Use default private folder").props.disabled).toBe(true);
});
it("shows an active grant restore error independently of a newer authorized pick",async()=>{
  selection.persisted=true;selection.activated=true;
  Object.assign(state,{mode:"custom",authorization_error:"Saved library authorization could not be restored"});
  await mount(<HarmonyModelStorage/>);
  expect(tree!.root.findByProps({role:"alert"}).children.join("")).toContain("could not be restored");
  expect(button("Use default private folder").props.disabled).toBe(false);
  delete (state as typeof state & {authorization_error?:string}).authorization_error;
});
it("requires confirmation and passes a selection id, never an arbitrary target path", async()=>{
  selection.persisted=true;selection.activated=true;
  await mount(<HarmonyModelStorage/>);
  await act(async()=>button("Migrate model library").props.onClick());
  expect(tree!.root.findByProps({role:"dialog"})).toBeDefined();
  await act(async()=>button("Confirm migration").props.onClick());
  expect(fetch).toHaveBeenCalledWith("/api/harmony/storage/migrate",expect.objectContaining({method:"POST",body:'{"selection_id":"selection","confirm":true}'}));
});
it("reports recovery errors and removes polling on unmount", async()=>{
  await mount(<HarmonyModelStorage/>);
  vi.mocked(fetch).mockResolvedValueOnce({ok:false,text:async()=>"identity mismatch"} as Response);
  await act(async()=>button("Recover interrupted migration").props.onClick());
  expect(tree!.root.findByProps({role:"alert"}).children.join("")).toContain("identity mismatch");
  await act(async()=>tree!.unmount());tree=undefined;expect(vi.getTimerCount()).toBe(0);
});
it("exports through the narrow native route with an encoded model, not a file path",async()=>{
  await mount(<HarmonyModelExport model="library/model:tag" disabled={false}/>);
  await act(async()=>button("Export GGUF").props.onClick());
  expect(window.location.href).toBe("harmony-ollama://export-gguf?model=library%2Fmodel%3Atag");
  expect(fetch).not.toHaveBeenCalled();
});
it("handles progress and cancellation, but does not cancel publication",async()=>{
  await mount(<HarmonyModelExport model="model:tag" disabled={false}/>);
  const update=async(phase:string)=>act(async()=>listener!({detail:{model:"model:tag",status:"running",phase,error:"",bytes:32,total:64}} as unknown as Event));
  await update("receiving");expect(button("Export GGUF").props.disabled).toBe(true);
  await act(async()=>button("Cancel export").props.onClick());
  expect(window.location.href).toBe("harmony-ollama://cancel-model-export");
  await update("publishing");expect(button("Cancel export")).toBeUndefined();
  await act(async()=>tree!.unmount());tree=undefined;expect(window.removeEventListener).toHaveBeenCalled();
});
