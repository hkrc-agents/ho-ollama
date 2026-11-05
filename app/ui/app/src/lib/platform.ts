export function isWindowsPlatform(): boolean {
  if (typeof window !== "undefined" && window.OLLAMA_PLATFORM) {
    return window.OLLAMA_PLATFORM === "windows";
  }

  return (
    typeof navigator !== "undefined" &&
    navigator.platform.toLowerCase().includes("win")
  );
}

// Desktop integrations require macOS capabilities, not merely "not Windows".
export function isMacOSPlatform(): boolean {
  if (typeof window !== "undefined" && window.OLLAMA_PLATFORM) {
    return window.OLLAMA_PLATFORM === "darwin";
  }
  return (
    typeof navigator !== "undefined" &&
    navigator.platform.toLowerCase().includes("mac")
  );
}

// Preserve the upstream non-Windows UI on every other platform, including
// browser-based Linux development. Only HarmonyOS lacks this integration.
export function supportsDesktopIntegrations(): boolean {
  return !(typeof window !== "undefined" && window.OLLAMA_PLATFORM === "harmonyos")
    && !isWindowsPlatform();
}
