import { isWindowsPlatform } from "./platform";

// Layout capability, not OS identity. HarmonyOS draws its own native title bar.
export function usesNativeWindowChrome(): boolean {
  return (typeof window !== "undefined" && window.OLLAMA_PLATFORM === "harmonyos") || isWindowsPlatform();
}
