import { afterEach, describe, expect, it, vi } from "vitest";
import { isMacOSPlatform, isWindowsPlatform, supportsDesktopIntegrations } from "./platform";

afterEach(() => vi.unstubAllGlobals());
describe("native platform capability boundaries", () => {
  it.each(["Linux x86_64", "MacIntel", "Win32", ""])("preserves the upstream desktop predicate for %s", platform => {
    vi.stubGlobal("window", {});
    vi.stubGlobal("navigator", { platform });
    expect(supportsDesktopIntegrations()).toBe(!isWindowsPlatform());
  });
  it("does not impersonate Windows or expose macOS integrations on HarmonyOS", () => {
    vi.stubGlobal("window", { OLLAMA_PLATFORM: "harmonyos" });
    vi.stubGlobal("navigator", { platform: "MacIntel" });
    expect(isMacOSPlatform()).toBe(false);
    expect(isWindowsPlatform()).toBe(false);
    expect(supportsDesktopIntegrations()).toBe(false);
  });
  it("preserves macOS desktop integrations", () => {
    vi.stubGlobal("window", { OLLAMA_PLATFORM: "darwin" });
    expect(isMacOSPlatform()).toBe(true);
    expect(isWindowsPlatform()).toBe(false);
  });
  it("keeps macOS integrations hidden on Windows", () => {
    vi.stubGlobal("window", { OLLAMA_PLATFORM: "windows" });
    expect(isMacOSPlatform()).toBe(false);
    expect(isWindowsPlatform()).toBe(true);
  });
});
