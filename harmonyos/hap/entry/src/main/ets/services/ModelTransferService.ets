import { common } from '@kit.AbilityKit';
import { picker, fileIo as fs } from '@kit.CoreFileKit';
import http from '@ohos.net.http';
import hash from '@ohos.file.hash';
import statvfs from '@ohos.file.statvfs';
import util from '@ohos.util';

interface ExportMetadata { digest: string; size: number; manifest: string; }
export interface ModelTransferState {
  model: string; status: string; phase: string; bytes: number; total: number;
  error: string; uri: string; temporary: string;
}

// Only the actual native system picker supplies an output URI. The webpage
// supplies a model NAME, never a filesystem path. No arbitrary-file native API.
export class ModelTransferService {
  private static active: http.HttpRequest | undefined = undefined;
  private static busy: boolean = false;
  private static cancelled: boolean = false;
  private static publishing: boolean = false;

  static cancel(): boolean {
    if (ModelTransferService.publishing) return false;
    ModelTransferService.cancelled = true;
    ModelTransferService.active?.destroy();
    return true;
  }

  private static save(context: common.UIAbilityContext, state: ModelTransferState): void {
    const path = context.filesDir + '/model-export-state.json';
    const temporary = path + '.tmp-' + util.generateRandomUUID();
    const file = fs.openSync(temporary, fs.OpenMode.CREATE | fs.OpenMode.WRITE_ONLY | fs.OpenMode.NOFOLLOW);
    try {
      const data = new util.TextEncoder().encodeInto(JSON.stringify(state));
      let written: number = 0;
      while (written < data.byteLength) {
        const n = fs.writeSync(file.fd, data.slice(written).buffer);
        if (n <= 0 || n > data.byteLength - written) throw new Error('Incomplete export journal write');
        written += n;
      }
      fs.fsyncSync(file.fd);
    } finally { fs.closeSync(file); }
    fs.renameSync(temporary, path);
    const directory = fs.openSync(context.filesDir, fs.OpenMode.READ_ONLY | fs.OpenMode.NOFOLLOW);
    try { fs.fsyncSync(directory.fd); } finally { fs.closeSync(directory); }
  }

  static restore(context: common.UIAbilityContext): ModelTransferState | undefined {
    let file: fs.File;
    try { file = fs.openSync(context.filesDir + '/model-export-state.json', fs.OpenMode.READ_ONLY | fs.OpenMode.NOFOLLOW); }
    catch (_) { return undefined; }
    let state: ModelTransferState;
    try {
      const info = fs.statSync(file.fd);
      if (!info.isFile() || info.size > 16384) throw new Error('Invalid export journal');
      const data = new ArrayBuffer(info.size);
      let position: number = 0;
      while (position < data.byteLength) {
        const part = new ArrayBuffer(data.byteLength - position);
        const n = fs.readSync(file.fd, part);
        if (n <= 0 || n > part.byteLength) throw new Error('Incomplete export journal');
        new Uint8Array(data).set(new Uint8Array(part, 0, n), position); position += n;
      }
      state = JSON.parse(new util.TextDecoder().decodeToString(new Uint8Array(data))) as ModelTransferState;
      if (typeof state.model !== 'string' || typeof state.status !== 'string' || typeof state.temporary !== 'string') throw new Error('Invalid export journal');
    } finally { fs.closeSync(file); }
    if (ModelTransferService.busy) return state;
    if (state.status === 'running') {
      state.status = 'interrupted';
      state.error = state.phase === 'publishing' ?
        'Export publication was interrupted. Review the chosen output file; it is not a confirmed GGUF.' :
        'Export interrupted before confirmation. The chosen output may be empty.';
      ModelTransferService.save(context, state);
    }
    // No recursive/cache glob deletion. Only the exact private directory named
    // in our bounded receipt is eligible; unknown entries make rmdir fail closed.
    const prefix = context.cacheDir + '/ollama-export-';
    if (state.temporary.startsWith(prefix) && /^[0-9a-f-]{36}$/.test(state.temporary.substring(prefix.length))) {
      try { fs.unlinkSync(state.temporary + '/weights.part'); } catch (_) {}
      try { fs.rmdirSync(state.temporary); state.temporary = ''; ModelTransferService.save(context, state); } catch (_) {}
    }
    return state;
  }

  private static async digest(fd: number, cancelled: boolean): Promise<string> {
    fs.lseek(fd, 0, fs.WhenceType.SEEK_SET);
    const digest = hash.createHash('sha256');
    const buffer = new ArrayBuffer(128 * 1024);
    while (true) {
      if (cancelled && ModelTransferService.cancelled) throw new Error('Export cancelled');
      const n = await fs.read(fd, buffer);
      if (n === 0) break;
      digest.update(n === buffer.byteLength ? buffer : buffer.slice(0, n));
    }
    return digest.digest().toLowerCase();
  }

  static async export(context: common.UIAbilityContext, port: number, token: string, model: string,
    notify: (state: ModelTransferState) => void): Promise<void> {
    if (ModelTransferService.busy) throw new Error('Another export is active');
    if (!model || model.length > 512 || /[\x00-\x1f]/.test(model)) throw new Error('Invalid model name');
    ModelTransferService.busy = true; ModelTransferService.cancelled = false;
    const state: ModelTransferState = { model, status: 'running', phase: 'choosing', bytes: 0, total: 0,
      error: '', uri: '', temporary: '' };
    let staging: fs.File | undefined = undefined;
    let output: fs.File | undefined = undefined;
    let createdTemporary: boolean = false;
    const update = (phase: string): void => { state.phase = phase; ModelTransferService.save(context, state); notify(state); };
    try {
      update('checking');
      const metadataRequest = http.createHttp();
      let metadata: ExportMetadata;
      try {
        const response = await metadataRequest.request(`http://127.0.0.1:${port}/api/harmony/gguf/metadata?model=${encodeURIComponent(model)}`,
          { method: http.RequestMethod.GET, header: { Cookie: 'token=' + token }, expectDataType: http.HttpDataType.STRING, readTimeout: 60000 });
        if (response.responseCode !== 200) throw new Error(String(response.result));
        metadata = JSON.parse(String(response.result)) as ExportMetadata;
      } finally { metadataRequest.destroy(); }
      if (!/^sha256:[0-9a-f]{64}$/.test(metadata.digest) || !Number.isSafeInteger(metadata.size) || metadata.size < 24) throw new Error('Invalid export metadata');
      if (ModelTransferService.cancelled) throw new Error('Export cancelled');
      state.total = metadata.size;
      if (await statvfs.getFreeSize(context.cacheDir) < 2 * metadata.size + 256 * 1024 * 1024) throw new Error('Not enough space for staging and output');
      const options = new picker.DocumentSaveOptions();
      options.newFileNames = [model.replace(/[^A-Za-z0-9._-]/g, '-').substring(0, 120) + '-' + util.generateRandomUUID().substring(0, 8) + '.gguf'];
      options.fileSuffixChoices = ['GGUF|.gguf'];
      update('choosing');
      const uris = await new picker.DocumentViewPicker(context).save(options);
      if (uris.length === 0) { state.status = 'cancelled'; update('cancelled'); return; }
      if (ModelTransferService.cancelled) throw new Error('Export cancelled');
      state.uri = uris[0];
      state.temporary = context.cacheDir + '/ollama-export-' + util.generateRandomUUID();
      fs.mkdirSync(state.temporary);
      createdTemporary = true;
      staging = fs.openSync(state.temporary + '/weights.part', fs.OpenMode.CREATE | fs.OpenMode.READ_WRITE | fs.OpenMode.NOFOLLOW);
      update('downloading');
      const req = http.createHttp(); ModelTransferService.active = req;
      let streamError: string = '';
      let lastProgress: number = 0;
      req.on('dataReceive', (data: ArrayBuffer) => {
        try {
          if (staging === undefined || ModelTransferService.cancelled) throw new Error('Export cancelled');
          let offset: number = 0;
          while (offset < data.byteLength) {
            const n = fs.writeSync(staging.fd, offset === 0 ? data : data.slice(offset));
            if (n <= 0 || n > data.byteLength - offset) throw new Error('Incomplete export staging write');
            offset += n;
          }
          state.bytes += data.byteLength;
          if (state.bytes > state.total) throw new Error('Export exceeded expected size');
          if (Date.now() - lastProgress > 500) { lastProgress = Date.now(); notify(state); }
        } catch (error) { streamError = String(error); req.destroy(); }
      });
      try {
        const code = await req.requestInStream(`http://127.0.0.1:${port}/api/harmony/gguf/export?model=${encodeURIComponent(model)}`,
          { method: http.RequestMethod.GET, header: { Cookie: 'token=' + token }, readTimeout: 240000 });
        if (code !== 200 || streamError || state.bytes !== state.total) throw new Error(streamError || 'Incomplete export response');
      } finally { req.destroy(); ModelTransferService.active = undefined; }
      fs.fsyncSync(staging.fd);
      update('verifying');
      if (await ModelTransferService.digest(staging.fd, true) !== metadata.digest.substring(7)) throw new Error('Staged GGUF checksum mismatch');
      if (ModelTransferService.cancelled) throw new Error('Export cancelled');
      // Commit is not cancellable. Refuse nonempty destinations, even if the
      // system picker offered overwrite; existing user data is never truncated.
      ModelTransferService.publishing = true;
      update('publishing');
      output = fs.openSync(state.uri, fs.OpenMode.CREATE | fs.OpenMode.READ_WRITE | fs.OpenMode.NOFOLLOW);
      const outputInfo = fs.statSync(output.fd);
      if (!outputInfo.isFile() || outputInfo.size !== 0) throw new Error('Choose a new/empty output file; existing files are not overwritten');
      fs.lseek(staging.fd, 0, fs.WhenceType.SEEK_SET);
      await fs.copyFile(staging.fd, output.fd);
      fs.fsyncSync(output.fd);
      if (fs.statSync(output.fd).size !== metadata.size || await ModelTransferService.digest(output.fd, false) !== metadata.digest.substring(7)) {
        throw new Error('Output GGUF verification failed; output is not confirmed');
      }
      state.status = 'completed'; update('completed');
    } catch (error) {
      state.status = ModelTransferService.cancelled && !ModelTransferService.publishing ? 'cancelled' : 'failed';
      state.error = String(error);
      // A previously nonempty URI was never written. A newly published partial
      // file is retained with an explicit failure receipt, never called success.
      try { update(state.status); } catch (_) { notify(state); }
      throw new Error(String(error));
    } finally {
      try { if (output !== undefined) fs.closeSync(output); } catch (_) { state.error += ' Output close requires review.'; }
      try { if (staging !== undefined) fs.closeSync(staging); } catch (_) { state.error += ' Staging close requires review.'; }
      if (createdTemporary && state.temporary) {
        try { fs.unlinkSync(state.temporary + '/weights.part'); fs.rmdirSync(state.temporary); state.temporary = ''; ModelTransferService.save(context, state); }
        catch (_) { state.error += ' Private export cache cleanup pending.'; notify(state); }
      }
      ModelTransferService.busy = false; ModelTransferService.publishing = false;
    }
  }
}
