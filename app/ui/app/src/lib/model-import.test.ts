import { afterEach, describe, expect, it, vi } from "vitest";
import { importGGUF } from "./model-import";

afterEach(() => vi.unstubAllGlobals());
// Blob models the native File payload without requiring a DOM File constructor.
function file(name = "test.gguf", size = 32): File {
  const value = new Blob([new Uint8Array(size)]);
  Object.defineProperty(value, "name", { value: name });
  return value as File;
}
describe("GGUF adapter reuses Ollama create protocol", () => {
  it("sends the File object unchanged, not base64/JSON or an entire JS buffer", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response('{"status":"parsing","model":"new:latest"}\n{"status":"success","model":"new:latest"}\n')));
    const source = file(); const progress = vi.fn();
    expect(await importGGUF(source, new AbortController().signal, progress)).toBe("new:latest");
    expect(fetch).toHaveBeenCalledWith("/api/harmony/gguf", expect.objectContaining({
      method: "POST", body: source, headers: {"Content-Type":"application/octet-stream","X-GGUF-Size":"32"},
    }));
    expect(progress).toHaveBeenCalledTimes(2);
  });
  it("requires explicit success and retains a name reported before an error", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response('{"status":"parsing","model":"new:latest"}\n{"error":"result unconfirmed"}\n')));
    const progress = vi.fn();
    await expect(importGGUF(file(), new AbortController().signal, progress)).rejects.toThrow("unconfirmed");
    expect(progress).toHaveBeenCalledWith(expect.objectContaining({model:"new:latest"}));
  });
  it("does not count premature EOF as completion", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response('{"status":"parsing","model":"new:latest"}\n')));
    await expect(importGGUF(file(), new AbortController().signal, vi.fn())).rejects.toThrow("before Ollama confirmed");
  });
  it("rejects invalid extension or short files before network access", async () => {
    vi.stubGlobal("fetch", vi.fn());
    for (const source of [file("test.txt"), file("tiny.gguf", 4)]) {
      await expect(importGGUF(source, new AbortController().signal, vi.fn())).rejects.toThrow("Choose one GGUF");
    }
    expect(fetch).not.toHaveBeenCalled();
  });
  it("does not claim cancellation rolls back core", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response('{"status":"success","model":"new:latest"}\n')));
    const control = new AbortController(); control.abort();
    await expect(importGGUF(file(), control.signal, vi.fn())).rejects.toThrow("cancelled");
  });
});
