import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LoginStartup as HarmonyLoginStartup, HarmonyNotificationSettings } from "./login-startup";
import { readFileSync } from "node:fs";

// Unit-test state/IPC logic without requiring react-aria's real DOM focus setup.
// The actual shared controls are exercised in the frontend build and device UI run.
vi.mock("@/components/ui/button", () => ({ Button: (props: object) => <button {...props} /> }));
vi.mock("@/components/ui/fieldset", () => ({
  Field: (props: object) => <div {...props} />,
  Label: (props: object) => <label {...props} />,
  Description: (props: object) => <p {...props} />,
}));

let win: EventTarget & { location: { href: string } };
let doc: EventTarget & { hidden: boolean };
let tree: ReactTestRenderer | undefined;
function notify(status: unknown) {
  win.dispatchEvent(Object.assign(new Event("harmony-login-startup-state"), { detail: { status } }));
}
beforeEach(() => {
  vi.useFakeTimers();
  win = Object.assign(new EventTarget(), { location: { href: "" },
    setInterval, clearInterval, setTimeout, clearTimeout });
  doc = Object.assign(new EventTarget(), { hidden: false });
  vi.stubGlobal("window", win);
  vi.stubGlobal("document", doc);
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
});

describe("HarmonyOS notification settings", () => {
  const notifySettings = (notifications: unknown, serviceBanners: unknown) =>
    win.dispatchEvent(Object.assign(new Event("harmony-notification-state"), { detail: { notifications, serviceBanners } }));
  it("reads system state and distinguishes channel banners from global settings", async () => {
    await act(async () => { tree = create(<HarmonyNotificationSettings />); });
    expect(text()).toContain("Checking…");
    expect(text()).toContain("system-wide banner switch");
    await act(async () => vi.advanceTimersByTime(250));
    expect(win.location.href).toBe("harmony-ollama://notification-status");
    await act(async () => notifySettings("enabled", "disabled"));
    expect(text()).toContain("Notifications: ",);
    expect(text()).toContain("Enabled"); expect(text()).toContain("Disabled");
    await act(async () => notifySettings("disabled", "unknown"));
    expect(text()).toContain("Not available");
  });
  it("opens only the system page and cancels all timers on unmount", async () => {
    await act(async () => { tree = create(<HarmonyNotificationSettings />); });
    await act(async () => tree!.root.findByType("button").props.onClick());
    expect(win.location.href).toBe("harmony-ollama://notification-settings");
    await act(async () => tree!.unmount()); tree = undefined;
    expect(vi.getTimerCount()).toBe(0);
  });
  it("shows bridge errors instead of pretending notifications are enabled", async () => {
    await act(async () => { tree = create(<HarmonyNotificationSettings />); });
    await act(async () => vi.advanceTimersByTime(4000));
    expect(text()).toContain("Unable to read system setting");
    await act(async () => notifySettings("toString", true));
    expect(text()).not.toContain("Enabled");
  });
});
afterEach(async () => {
  if (tree) await act(async () => tree!.unmount());
  tree = undefined;
  vi.unstubAllGlobals(); vi.useRealTimers();
});
async function mount() { await act(async () => { tree = create(<HarmonyLoginStartup />); }); }
function text() { return JSON.stringify(tree!.toJSON()); }
describe("HarmonyOS system login startup status", () => {
  it("places the status on the title row, aligned to its right edge", async () => {
    await mount();
    const status = tree!.root.findByProps({ role: "status" });
    expect(status.props.className).toContain("ml-auto text-right");
    expect(status.parent!.props.className).toContain("flex items-center justify-between");
    expect(status.parent!.findByType("label").children).toContain("Start Ollama at login");
  });
  it("has one upstream Settings integration and no legacy duplicate card", () => {
    const source = readFileSync(new URL("../components/Settings.tsx", import.meta.url), "utf8");
    expect(source.match(/<HarmonyLoginStartup \/>/g)).toHaveLength(1);
    expect(source).not.toContain("Check the actual system status before enabling.");
  });
  it("requests the real setting and never defaults to enabled or disabled", async () => {
    await mount();
    expect(win.location.href).toBe("harmony-ollama://login-startup-status");
    expect(text()).toContain("Checking…");
    expect(text()).not.toContain("Enabled");
  });
  it("shows enabled then disabled without reopening the settings page", async () => {
    await mount();
    await act(async () => notify("enabled")); expect(text()).toContain("Enabled");
    await act(async () => notify("disabled")); expect(text()).toContain("Disabled");
  });
  it("distinguishes unsupported and query errors", async () => {
    await mount();
    await act(async () => notify("unsupported")); expect(text()).toContain("Not supported");
    await act(async () => notify("error")); expect(text()).toContain("Unable to read");
  });
  it("does not accept unknown state names", async () => {
    await mount();
    await act(async () => { notify("enabled"); notify("toString"); notify(true); });
    expect(text()).toContain("Enabled");
  });
  it("refreshes on focus and periodically, but not while the document is hidden", async () => {
    await mount(); win.location.href = "";
    await act(async () => vi.advanceTimersByTime(5000));
    expect(win.location.href).toContain("login-startup-status");
    win.location.href = ""; doc.hidden = true;
    await act(async () => vi.advanceTimersByTime(5000)); expect(win.location.href).toBe("");
    doc.hidden = false; win.dispatchEvent(new Event("focus"));
    expect(win.location.href).toContain("login-startup-status");
  });
  it("shows a timeout instead of leaving a broken native bridge checking forever", async () => {
    await mount(); await act(async () => vi.advanceTimersByTime(4000));
    expect(text()).toContain("Unable to read");
    await act(async () => notify("disabled")); expect(text()).toContain("Disabled");
  });
  it("only opens management on explicit user action and releases timers", async () => {
    await mount();
    await act(async () => tree!.root.findByType("button").props.onClick());
    expect(win.location.href).toBe("harmony-ollama://login-startup");
    await act(async () => tree!.unmount()); tree = undefined;
    expect(vi.getTimerCount()).toBe(0);
  });
});
