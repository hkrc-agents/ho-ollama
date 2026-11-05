import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SidebarLayout } from "@/components/layout/layout";
import { usesNativeWindowChrome } from "./window-layout";

vi.mock("@tanstack/react-router", () => ({ Link: (props: object) => <a {...props} /> }));
afterEach(() => vi.unstubAllGlobals());
describe("native window layout", () => {
  it("uses identical collapsed layout on Windows and HarmonyOS", () => {
    function html(platform: string) {
      vi.stubGlobal("window", { OLLAMA_PLATFORM: platform });
      return renderToStaticMarkup(<SidebarLayout title="Settings" sidebar={<nav />}><div /></SidebarLayout>);
    }
    const windows = html("windows");
    expect(html("harmonyos")).toEqual(windows);
    expect(windows).toContain("left-2"); expect(windows).toContain("pl-16");
    expect(html("darwin")).toContain("left-20");
  });
  it("keeps the expanded button at the left and restores the collapsed state", async () => {
    vi.stubGlobal("window", { OLLAMA_PLATFORM: "harmonyos" });
    let tree: ReactTestRenderer;
    await act(async () => { tree = create(<SidebarLayout title="Models" sidebar={<nav />} />); });
    await act(async () => { tree!.root.findByProps({"aria-label":"Show sidebar"}).props.onClick(); });
    expect(JSON.stringify(tree!.toJSON())).toContain("left-2");
    expect(JSON.stringify(tree!.toJSON())).not.toContain("left-[140px]");
    await act(async () => { tree!.root.findByProps({"aria-label":"Hide sidebar"}).props.onClick(); });
    await act(async () => { tree!.unmount(); });
  });
  it("does not require navigator or impersonate Windows", () => {
    vi.stubGlobal("window", { OLLAMA_PLATFORM: "harmonyos" });
    vi.stubGlobal("navigator", { platform: "Linux x86_64" });
    expect(usesNativeWindowChrome()).toBe(true);
    vi.stubGlobal("window", { OLLAMA_PLATFORM: "darwin" });
    expect(usesNativeWindowChrome()).toBe(false);
  });
});
