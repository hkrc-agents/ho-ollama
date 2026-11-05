// Host behavior tests of the real ETS source with an in-memory SDK boundary.
// Mock policy responses are not device permission qualification.
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { TextDecoder, TextEncoder } from "node:util";
import ts from "typescript";
import { beforeEach, expect, it, vi } from "vitest";

const source = readFileSync(process.env.PICO_MODEL_STORAGE_SOURCE || new URL("../hap/entry/src/main/ets/services/ModelStorageService.ets", import.meta.url), "utf8");
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } });
const filesDir = "/sandbox/files";
const selectionFile = filesDir + "/model-directory-selection.json";
const journalFile = filesDir + "/desktop/storage-migration.json";
const receiptFile = filesDir + "/model-directory-active-restore.json";
const id = "a".repeat(32);
const grant = (name: string) => ({ schema: 1, id: "storage-" + name, uri: "file:///" + name, path: "/" + name, operation_mode: 31, persisted: true, activated: true, time: 1 });
const root = (name: string) => "/" + name + "/ollama-models-" + id;
let files: Map<string, Uint8Array>;
let service: { restore(context: object): Promise<void>; select(context: object): Promise<void> };
let policies: { checkPersistentPermission: ReturnType<typeof vi.fn>; activatePermission: ReturnType<typeof vi.fn>; persistPermission: ReturnType<typeof vi.fn> };
let statuses: Map<string, string>;
let pickedUris: string[];
const put = (path: string, value: unknown) => files.set(path, new TextEncoder().encode(JSON.stringify(value)));
const get = (path: string) => JSON.parse(new TextDecoder().decode(files.get(path)));
const journal = (state: string = "completed") => ({ version: 1, state, old_root: root("old"), new_root: root("active"), old_selection: grant("old"), new_selection: grant("active") });
const restored = () => policies.activatePermission.mock.calls.map(([p]) => p[0].uri);

beforeEach(() => {
  files = new Map(); statuses = new Map(); pickedUris = [];
  policies = { checkPersistentPermission: vi.fn(async () => [true]), activatePermission: vi.fn(async () => {}), persistPermission: vi.fn(async () => {}) };
  let sequence = 0;
  const handles = new Map<number, { path: string; offset: number }>();
  const fs = {
    OpenMode: { READ_ONLY: 0, WRITE_ONLY: 1, CREATE: 2, TRUNC: 4, NOFOLLOW: 8 },
    openSync(path: string, mode: number) {
      if (mode & 2) files.set(path, new Uint8Array());
      if (!files.has(path)) throw { code: 13900002 };
      const fd = ++sequence; handles.set(fd, { path, offset: 0 }); return { fd };
    },
    closeSync(file: { fd: number }) { handles.delete(file.fd); },
    statSync(fd: number) { return { isFile: () => true, size: files.get(handles.get(fd)!.path)!.byteLength }; },
    readSync(fd: number, buffer: ArrayBuffer) {
      const h = handles.get(fd)!; const chunk = files.get(h.path)!.slice(h.offset, h.offset + buffer.byteLength);
      new Uint8Array(buffer).set(chunk); h.offset += chunk.byteLength; return chunk.byteLength;
    },
    writeSync(fd: number, buffer: ArrayBuffer) {
      const h = handles.get(fd)!; const old = files.get(h.path)!; const bytes = new Uint8Array(old.length + buffer.byteLength);
      bytes.set(old); bytes.set(new Uint8Array(buffer), old.length); files.set(h.path, bytes); return buffer.byteLength;
    },
    fsyncSync() {},
    renameSync(from: string, to: string) { files.set(to, files.get(from)!); files.delete(from); },
    unlinkSync(path: string) { files.delete(path); },
  };
  const exports = {};
  runInNewContext(compiled.outputText, {
    exports, AppStorage: { setOrCreate: (key: string, value: string) => statuses.set(key, value) },
    require(name: string) {
      if (name === "@kit.CoreFileKit") return { fileIo: fs, fileShare: { ...policies, OperationMode: { READ_MODE: 1, WRITE_MODE: 2, CREATE_MODE: 4, DELETE_MODE: 8, RENAME_MODE: 16 } },
        fileUri: { FileUri: class { path: string; constructor(uri: string) { this.path = new URL(uri).pathname; } } },
        picker: { DocumentSelectOptions: class {}, DocumentSelectMode: { FOLDER: 1 }, DocumentViewPicker: class { async select() { return pickedUris; } } } };
      if (name === "@kit.PerformanceAnalysisKit") return { hilog: { info() {}, warn() {}, error() {} } };
      if (name === "@ohos.util") return { default: { generateRandomUUID: () => "test-uuid", TextEncoder: class { encodeInto(text: string) { return new TextEncoder().encode(text); } }, TextDecoder: class { decodeToString(bytes: Uint8Array) { return new TextDecoder().decode(bytes); } } } };
      if (name === "libollamabridge.so") return { default: {} };
      throw new Error("Unexpected import: " + name);
    },
  });
  service = (exports as { ModelStorageService: typeof service }).ModelStorageService;
});

it("starts with the default sandbox without asking for a grant", async () => {
  await service.restore({ filesDir });
  expect(restored()).toEqual([]);
  expect(policies.persistPermission).not.toHaveBeenCalled();
  expect(statuses.get("modelStorageStatus")).toBe("unselected");
});

it("requests only the Picker read/write policy and records confirmed activation", async () => {
  pickedUris = ["file:///chosen"];
  await service.select({ filesDir });
  expect(policies.persistPermission).toHaveBeenCalledExactlyOnceWith([{ uri: "file:///chosen", operationMode: 3 }]);
  expect(policies.activatePermission).toHaveBeenCalledExactlyOnceWith([{ uri: "file:///chosen", operationMode: 3 }]);
  expect(get(selectionFile)).toMatchObject({ operation_mode: 3, persisted: true, activated: true, path: "/chosen" });
  expect(get(selectionFile).error).toBeUndefined();
});

it("does not retry or downgrade a denied Picker policy", async () => {
  pickedUris = ["file:///denied"];
  policies.persistPermission.mockRejectedValueOnce({ code: 13900001 });
  await service.select({ filesDir });
  expect(policies.persistPermission).toHaveBeenCalledTimes(1);
  expect(policies.activatePermission).not.toHaveBeenCalled();
  expect(get(selectionFile)).toMatchObject({ operation_mode: 3, persisted: false, activated: false, error: "persist:code=13900001" });
});

it("restores the committed active grant before a newer staged selection", async () => {
  put(journalFile, journal()); put(selectionFile, grant("staged"));
  const before = get(journalFile);
  await service.restore({ filesDir });
  expect(restored()).toEqual(["file:///active", "file:///staged"]);
  expect(get(journalFile)).toEqual(before);
  expect(get(selectionFile).path).toBe("/staged");
  expect(policies.persistPermission).not.toHaveBeenCalled();
});

it("restores the active grant after a cancelled staged pick", async () => {
  put(journalFile, journal());
  put(selectionFile, { ...grant("cancelled"), uri: "", path: "", persisted: false, activated: false, error: "selection_cancelled" });
  await service.restore({ filesDir });
  expect(restored()).toEqual(["file:///active"]);
});

it("restores both transaction grants before interrupted rollback", async () => {
  put(journalFile, journal("switching")); put(selectionFile, grant("staged"));
  await service.restore({ filesDir });
  expect(restored()).toEqual(["file:///old", "file:///active", "file:///staged"]);
});

it("restores the old grant after rollback without promoting the failed destination", async () => {
  put(journalFile, journal("rolled_back")); put(selectionFile, grant("staged"));
  await service.restore({ filesDir });
  expect(restored()).toEqual(["file:///old", "file:///staged"]);
});

it("does not trust persisted booleans after the OS revokes the active grant", async () => {
  put(journalFile, journal()); put(selectionFile, grant("staged"));
  policies.checkPersistentPermission.mockImplementation(async (p) => [p[0].uri !== "file:///active"]);
  await service.restore({ filesDir });
  expect(restored()).toEqual(["file:///staged"]);
  expect(get(receiptFile).new_error).toContain("could not be restored");
  expect(statuses.get("modelStorageStatus")).toBe("error");
  expect(get(journalFile).new_root).toBe(root("active"));
});

it("keeps failures independent when an interrupted transaction needs two grants", async () => {
  put(journalFile, journal("switching"));
  policies.activatePermission.mockRejectedValueOnce({ code: 123 });
  await service.restore({ filesDir });
  expect(restored()).toEqual(["file:///old", "file:///active"]);
  expect(get(receiptFile).old_error).toContain("code=123");
  expect(get(receiptFile).new_error).toBe("");
});

it("rejects a journal URI not bound to its dedicated model root", async () => {
  put(journalFile, { ...journal(), new_root: "/different-root" });
  await service.restore({ filesDir });
  expect(restored()).toEqual([]);
  expect(get(receiptFile).new_error).not.toBe("");
});

it("does not reactivate the custom journal grant after a completed private return", async () => {
  put(journalFile, { ...journal(), new_root: filesDir + "/models", new_selection: undefined });
  await service.restore({ filesDir });
  expect(restored()).toEqual([]);
});

it("Picker cancellation leaves the committed journal and model selection intact", async () => {
  put(journalFile, journal()); put(selectionFile, grant("staged"));
  const before = get(journalFile);
  await service.select({ filesDir });
  expect(get(journalFile)).toEqual(before);
  expect(get(selectionFile).error).toBe("selection_cancelled");
  expect(policies.persistPermission).not.toHaveBeenCalled();
});

it("retains the saved active root when its policy activation is denied", async () => {
  put(journalFile, journal());
  policies.activatePermission.mockRejectedValueOnce({ code: 456 });
  await service.restore({ filesDir });
  expect(get(receiptFile).new_error).toContain("code=456");
  expect(get(journalFile).new_root).toBe(root("active"));
  expect(files.has(selectionFile)).toBe(false);
});

it("reports corrupt authorization journals without inventing a private fallback", async () => {
  put(journalFile, { version: 1, state: "invented" });
  await service.restore({ filesDir });
  expect(get(receiptFile).error).toContain("recovery required");
  expect(restored()).toEqual([]);
  expect(statuses.get("modelStorageStatus")).toBe("error");
});
