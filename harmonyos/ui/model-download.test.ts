import { afterEach, describe, expect, it, vi } from "vitest";
import { downloadModel } from "./model-download";

afterEach(() => vi.unstubAllGlobals());
function reply(chunks: string[]) {
  const encoder = new TextEncoder();
  vi.stubGlobal("fetch", vi.fn(async () => new Response(new ReadableStream({
    start(controller) { for (const chunk of chunks) controller.enqueue(encoder.encode(chunk)); controller.close(); },
  }))));
}
describe("Ollama download protocol", () => {
  it("passes the original model and consumes fragmented progress and final line", async () => {
    reply(['{"status":"pulling",', '"completed":1,"total":2}\n{"status":"success"}']);
    const progress = vi.fn();
    await downloadModel("test:tag", new AbortController().signal, progress);
    expect(progress).toHaveBeenCalledTimes(2);
    expect(JSON.parse(vi.mocked(fetch).mock.calls[0][1]!.body as string)).toEqual({model:"test:tag",stream:true});
  });
  it("does not count premature EOF as success", async () => {
    reply(['{"status":"pulling"}\n']);
    await expect(downloadModel("test", new AbortController().signal, vi.fn())).rejects.toThrow("before Ollama confirmed");
  });
  it("reports HTTP-200 stream errors", async () => {
    reply(['{"error":"not enough disk space"}\n']);
    await expect(downloadModel("test", new AbortController().signal, vi.fn())).rejects.toThrow("not enough disk");
  });
  it("rejects malformed data", async () => {
    reply(['garbage\n']);
    await expect(downloadModel("test", new AbortController().signal, vi.fn())).rejects.toThrow();
  });
  it("does not turn cancellation into completion", async () => {
    reply(['{"status":"success"}\n']);
    const controller = new AbortController(); controller.abort();
    await expect(downloadModel("test", controller.signal, vi.fn())).rejects.toThrow("cancelled");
  });
  it("reports non-200 responses", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("Forbidden", {status:403})));
    await expect(downloadModel("test", new AbortController().signal, vi.fn())).rejects.toThrow("Forbidden");
  });
});
