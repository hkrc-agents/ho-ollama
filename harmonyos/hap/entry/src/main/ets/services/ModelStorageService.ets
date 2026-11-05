import { common } from '@kit.AbilityKit';
import { picker, fileUri, fileShare, fileIo as fs } from '@kit.CoreFileKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { BusinessError } from '@kit.BasicServicesKit';
import util from '@ohos.util';
import bridge from 'libollamabridge.so';

interface ModelDirectorySelection {
  schema: number;
  id: string;
  uri: string;
  path: string;
  operation_mode: number;
  persisted: boolean;
  activated: boolean;
  time: number;
  error?: string;
  checked_modes?: number[];
  checked_results?: boolean[];
  auth_mode?: boolean;
  native_audit_json?: string;
}

interface StorageMigrationGrant {
  version: number;
  state: string;
  old_root: string;
  new_root: string;
  old_selection?: ModelDirectorySelection;
  new_selection?: ModelDirectorySelection;
}

interface StorageRestoreState {
  schema: number;
  old_root: string;
  new_root: string;
  old_error: string;
  new_error: string;
  error: string;
}

// Picker authorization only. The Go migration journal owns the active library;
// a newer staged pick must never replace that library's startup grant.
export class ModelStorageService {
  private static operations: Promise<void> = Promise.resolve();
  private static readonly filename: string = '/model-directory-selection.json';
  private static readonly limit: number = 16384;
  // DocumentViewPicker grants the public read/write policy. Request that exact
  // policy, not extra fine-grained bits absent from the Picker grant. On the
  // ARM64 PC, mode31 persist fails while mode3 survives APP restart and passes
  // the full create/fsync/replace-rename/delete probe in APP and child actors.
  // This is not a retry/downgrade after denial: revoked/failed grants fail closed.
  private static readonly mode: number = fileShare.OperationMode.READ_MODE |
    fileShare.OperationMode.WRITE_MODE;

  static select(context: common.UIAbilityContext): Promise<void> {
    const operation = ModelStorageService.operations.then(() => ModelStorageService.pick(context));
    ModelStorageService.operations = operation.catch(() => {});
    return ModelStorageService.operations;
  }

  // Retain the existing debug diagnostic entry. The filesystem probe still
  // requires actual create/delete/replace-rename; policy booleans are not I/O.
  static selectReadWrite(context: common.UIAbilityContext): Promise<void> {
    const operation = ModelStorageService.operations.then(() => ModelStorageService.pick(context, 3));
    ModelStorageService.operations = operation.catch(() => {});
    return ModelStorageService.operations;
  }

  static authorizeSelected(context: common.UIAbilityContext): Promise<void> {
    const operation = ModelStorageService.operations.then(() => ModelStorageService.pick(context, ModelStorageService.mode, true));
    ModelStorageService.operations = operation.catch(() => {});
    return ModelStorageService.operations;
  }

  static restore(context: common.UIAbilityContext): Promise<void> {
    const operation = ModelStorageService.operations.then(async () => {
      let failed: boolean = false;
      const result: StorageRestoreState = { schema: 1, old_root: '', new_root: '', old_error: '', new_error: '', error: '' };
      try {
        const text = ModelStorageService.readText(context, '/desktop/storage-migration.json', 16 * 1024 * 1024);
        if (text !== undefined) {
          const journal = JSON.parse(text) as StorageMigrationGrant;
          const states = ['preparing', 'copying', 'verifying', 'switching', 'starting', 'completed',
            'rolled_back', 'recovery_required', 'discarded'];
          if (journal.version !== 1 || !states.includes(journal.state) ||
            typeof journal.old_root !== 'string' || !journal.old_root.startsWith('/') ||
            typeof journal.new_root !== 'string' || !journal.new_root.startsWith('/')) throw new Error('Invalid migration journal');
          result.old_root = journal.old_root;
          result.new_root = journal.new_root;
          // Interrupted transactions need both policies for deterministic rollback.
          const grants: ModelDirectorySelection[] = [];
          if (journal.state !== 'completed' && journal.old_selection !== undefined) grants.push(journal.old_selection);
          if (!['rolled_back', 'discarded'].includes(journal.state) && journal.new_selection !== undefined) grants.push(journal.new_selection);
          for (const grant of grants) {
            const old: boolean = grant === journal.old_selection;
            try {
              ModelStorageService.validate(grant);
              const expected = old ? journal.old_root : journal.new_root;
              if (!/^ollama-models-[a-f0-9]{32}$/.test(expected.slice(grant.path.length + 1)) ||
                !expected.startsWith(grant.path + '/')) throw new Error('Journal grant/root mismatch');
              await ModelStorageService.activateStored(grant);
            } catch (error) {
              failed = true;
              const message = 'Saved library authorization could not be restored at startup; reauthorize its folder and restart the app. code=' + ModelStorageService.code(error as BusinessError);
              if (old) result.old_error = message; else result.new_error = message;
            }
          }
        }
      } catch (error) {
        failed = true;
        result.error = 'Saved library authorization journal could not be restored; recovery required.';
        hilog.warn(0, 'OllamaStorage', 'active grant restore code=%{public}s', ModelStorageService.code(error as BusinessError));
      }
      try { ModelStorageService.atomic(context, '/model-directory-active-restore.json', result); }
      catch (error) {
        failed = true;
        hilog.warn(0, 'OllamaStorage', 'restore receipt code=%{public}s', ModelStorageService.code(error as BusinessError));
      }
      await ModelStorageService.reactivate(context);
      if (failed) ModelStorageService.status('error');
    });
    ModelStorageService.operations = operation.catch(() => {});
    return ModelStorageService.operations;
  }

  static auditNative(context: common.UIAbilityContext): Promise<string> {
    let result: string = '';
    const operation = ModelStorageService.operations.then(() => {
      const record = ModelStorageService.read(context);
      if (record === undefined || record.uri.length === 0) throw new Error('Choose a directory first');
      result = bridge.storagePermissionAudit(record.uri);
      if (result.length > 8192) throw new Error('Native audit exceeds limit');
      record.native_audit_json = result;
      ModelStorageService.save(context, record);
    });
    ModelStorageService.operations = operation.catch(() => {});
    return operation.then(() => result);
  }

  static retrySelected(context: common.UIAbilityContext): Promise<void> {
    const operation = ModelStorageService.operations.then(async () => {
      const record = ModelStorageService.read(context);
      if (record === undefined || record.uri.length === 0) throw new Error('Choose a directory first');
      record.persisted = false; record.activated = false;
      record.error = 'retry_pending';
      ModelStorageService.save(context, record);
      let stage: string = 'retry_persist';
      try {
        const policies: fileShare.PolicyInfo[] = [{ uri: record.uri, operationMode: record.operation_mode }];
        await fileShare.persistPermission(policies);
        stage = 'retry_check';
        const results = await fileShare.checkPersistentPermission(policies);
        record.persisted = results.length === 1 && results[0];
        record.checked_modes = [record.operation_mode];
        record.checked_results = results;
        if (!record.persisted) throw new Error('Persistent policy not confirmed');
        stage = 'retry_activate';
        await fileShare.activatePermission(policies);
        record.activated = true; record.error = undefined;
        ModelStorageService.save(context, record);
        ModelStorageService.status('policy_activated');
      } catch (error) { ModelStorageService.failure(context, record, stage, error as BusinessError, true); }
    });
    ModelStorageService.operations = operation.catch(() => {});
    return operation;
  }

  private static fresh(): ModelDirectorySelection {
    return { schema: 1, id: 'storage-' + util.generateRandomUUID(), uri: '', path: '',
      operation_mode: ModelStorageService.mode, persisted: false, activated: false, time: Date.now() };
  }

  private static code(error: BusinessError): string {
    return Number.isFinite(error.code) ? String(error.code) : 'unknown';
  }

  private static status(value: string): void {
    AppStorage.setOrCreate('modelStorageStatus', value);
  }

  private static path(uri: string): string {
    if (uri.length === 0 || uri.length > 8192 || !uri.startsWith('file://') || /[\x00-\x1f\x7f]/.test(uri)) {
      throw new Error('Invalid selected URI');
    }
    // Same native FileUri conversion as the physical folder-grant experiment.
    const path = decodeURIComponent(new fileUri.FileUri(uri).path);
    if (!path.startsWith('/') || path === '/' || path.length > 4096 || /[\x00-\x1f\x7f]/.test(path)) {
      throw new Error('Invalid selected path');
    }
    const segments = path.split('/');
    if (segments.some((part: string) => part === '.' || part === '..')) {
      throw new Error('Selected path contains traversal');
    }
    return path;
  }

  private static atomic(context: common.UIAbilityContext, suffix: string, record: ModelDirectorySelection | StorageRestoreState): void {
    const bytes = new util.TextEncoder().encodeInto(JSON.stringify(record));
    if (bytes.byteLength > ModelStorageService.limit) throw new Error('Metadata exceeds limit');
    const target = context.filesDir + suffix;
    const temporary = target + '.tmp-' + util.generateRandomUUID();
    try {
      const file = fs.openSync(temporary, fs.OpenMode.WRITE_ONLY | fs.OpenMode.CREATE |
        fs.OpenMode.TRUNC | fs.OpenMode.NOFOLLOW);
      try {
        let offset: number = 0;
        while (offset < bytes.byteLength) {
          const count = fs.writeSync(file.fd, bytes.slice(offset).buffer);
          if (count <= 0 || count > bytes.byteLength - offset) throw new Error('Incomplete metadata write');
          offset += count;
        }
        fs.fsyncSync(file.fd);
      } finally { fs.closeSync(file); }
      // Never unlink the old record first. Failed replacement preserves it.
      fs.renameSync(temporary, target);
    } finally {
      try { fs.unlinkSync(temporary); } catch (_) {}
    }
  }

  private static readText(context: common.UIAbilityContext, suffix: string, limit: number): string | undefined {
    const target = context.filesDir + suffix;
    let file: fs.File;
    try { file = fs.openSync(target, fs.OpenMode.READ_ONLY | fs.OpenMode.NOFOLLOW); }
    catch (error) {
      if ((error as BusinessError).code === 13900002) return undefined;
      throw error as Error;
    }
    let text: string;
    try {
      const stat = fs.statSync(file.fd);
      if (!stat.isFile() || stat.size <= 0 || stat.size > limit) throw new Error('Invalid metadata file');
      const bytes = new ArrayBuffer(stat.size + 1);
      let offset: number = 0;
      while (offset < bytes.byteLength) {
        const part = new ArrayBuffer(bytes.byteLength - offset);
        const count = fs.readSync(file.fd, part);
        if (count === 0) break;
        new Uint8Array(bytes).set(new Uint8Array(part, 0, count), offset);
        offset += count;
      }
      if (offset === 0 || offset > limit) throw new Error('Metadata exceeds limit');
      text = new util.TextDecoder().decodeToString(new Uint8Array(bytes, 0, offset));
    } finally { fs.closeSync(file); }
    return text;
  }

  private static read(context: common.UIAbilityContext): ModelDirectorySelection | undefined {
    const text = ModelStorageService.readText(context, ModelStorageService.filename, ModelStorageService.limit);
    if (text === undefined) return undefined;
    const record = JSON.parse(text) as ModelDirectorySelection;
    ModelStorageService.validate(record);
    return record;
  }

  private static validate(record: ModelDirectorySelection): void {
    if (record === null || record.schema !== 1 || typeof record.id !== 'string' ||
      !/^storage-[A-Za-z0-9-]{1,80}$/.test(record.id) || typeof record.uri !== 'string' ||
      typeof record.path !== 'string' || (record.operation_mode !== 3 && record.operation_mode !== 31) ||
      typeof record.persisted !== 'boolean' || typeof record.activated !== 'boolean' ||
      (record.activated && !record.persisted) ||
      typeof record.time !== 'number' || !Number.isFinite(record.time) || record.time <= 0 ||
      (record.error !== undefined && (typeof record.error !== 'string' || record.error.length > 256))) {
      throw new Error('Invalid metadata schema');
    }
    if (record.uri.length > 0) {
      if (ModelStorageService.path(record.uri) !== record.path) throw new Error('Stored URI/path mismatch');
    } else if (record.path !== '' || record.persisted || record.activated) {
      throw new Error('Invalid empty selection');
    }
  }

  private static async activateStored(record: ModelDirectorySelection): Promise<void> {
    const policies: fileShare.PolicyInfo[] = [{ uri: record.uri, operationMode: record.operation_mode }];
    // Persisted/activated fields describe a past process. Recheck with the OS.
    const checks = await fileShare.checkPersistentPermission(policies);
    if (checks.length !== 1 || !checks[0]) throw new Error('Persistent policy unavailable');
    await fileShare.activatePermission(policies);
  }

  private static save(context: common.UIAbilityContext, record: ModelDirectorySelection): void {
    record.time = Date.now();
    ModelStorageService.atomic(context, ModelStorageService.filename, record);
  }

  private static failure(context: common.UIAbilityContext, record: ModelDirectorySelection,
    stage: string, error: BusinessError, replace: boolean): void {
    record.error = stage + ':code=' + ModelStorageService.code(error);
    record.time = Date.now();
    ModelStorageService.status('error');
    // Error messages can contain paths/URIs; log only our finite stage and SDK code.
    hilog.warn(0, 'OllamaStorage', '%{public}s code=%{public}s', stage, ModelStorageService.code(error));
    try {
      ModelStorageService.atomic(context, replace ? ModelStorageService.filename :
        '/model-directory-selection-attempt.json', record);
    } catch (writeError) {
      hilog.error(0, 'OllamaStorage', 'metadata write code=%{public}s', ModelStorageService.code(writeError as BusinessError));
    }
  }

  private static async pick(context: common.UIAbilityContext, mode: number = ModelStorageService.mode,
    authorizeSelected: boolean = false): Promise<void> {
    const record = ModelStorageService.fresh();
    record.operation_mode = mode;
    let stage: string = 'selection_read';
    let replace: boolean = false;
    try {
      const previous = ModelStorageService.read(context);
      if (authorizeSelected) {
        if (previous === undefined || previous.uri.length === 0) throw new Error('Choose a directory first');
        record.operation_mode = previous.operation_mode;
        record.auth_mode = true;
      }
      // Preserve the last URI-bearing selection across cancellation/failed picks.
      if (previous !== undefined && previous.uri.length > 0) {
        stage = 'selection_backup';
        ModelStorageService.atomic(context, '/model-directory-selection.previous.json', previous);
      }
      replace = true;
      record.error = 'selection_pending';
      stage = 'selection_record';
      ModelStorageService.save(context, record);
      ModelStorageService.status('selecting');
      stage = 'picker';
      const options = new picker.DocumentSelectOptions();
      options.selectMode = picker.DocumentSelectMode.FOLDER;
      options.maxSelectNumber = 1;
      if (authorizeSelected && previous !== undefined) {
        // Ask for visible system authorization of the URI already selected by
        // this APP. Never accept a target URI/path from an external Want.
        options.authMode = true;
        options.defaultFilePathUri = previous.uri;
      }
      const uris = await new picker.DocumentViewPicker(context).select(options);
      if (uris.length === 0) {
        record.error = 'selection_cancelled';
        ModelStorageService.save(context, record);
        ModelStorageService.status('cancelled');
        return;
      }
      stage = 'selection_validate';
      if (uris.length !== 1) throw new Error('Expected one selected folder');
      const path = ModelStorageService.path(uris[0]);
      record.uri = uris[0];
      record.path = path;
      record.error = 'authorization_pending';
      ModelStorageService.save(context, record);
      const policies: fileShare.PolicyInfo[] = [{ uri: record.uri, operationMode: record.operation_mode }];
      stage = 'persist';
      await fileShare.persistPermission(policies);
      stage = 'check';
      const checks = await fileShare.checkPersistentPermission(policies);
      record.persisted = checks.length === 1 && checks[0];
      record.checked_modes = [1, 2, 3, 4, 8, 16, 31];
      record.checked_results = await fileShare.checkPersistentPermission(record.checked_modes.map(
        (value: number): fileShare.PolicyInfo => { return { uri: record.uri, operationMode: value }; }));
      if (!record.persisted) throw new Error('Persistent policy not confirmed');
      record.error = 'activation_pending';
      ModelStorageService.save(context, record);
      stage = 'activate';
      await fileShare.activatePermission(policies);
      record.activated = true;
      record.error = undefined;
      stage = 'authorization_record';
      ModelStorageService.save(context, record);
      ModelStorageService.status('policy_activated');
      hilog.info(0, 'OllamaStorage', 'selection policy activated; filesystem operations unverified');
    } catch (error) { ModelStorageService.failure(context, record, stage, error as BusinessError, replace); }
  }

  private static async reactivate(context: common.UIAbilityContext): Promise<void> {
    let record = ModelStorageService.fresh();
    let stage: string = 'restore_read';
    let replace: boolean = false;
    try {
      const stored = ModelStorageService.read(context);
      if (stored === undefined) { ModelStorageService.status('unselected'); return; }
      record = stored;
      if (record.uri.length === 0) {
        ModelStorageService.status(record.error === 'selection_cancelled' ? 'cancelled' :
          (record.error !== undefined ? 'error' : 'unselected'));
        return;
      }
      replace = true;
      record.persisted = false;
      record.activated = false;
      record.error = 'restore_check_pending';
      stage = 'restore_record';
      ModelStorageService.save(context, record);
      const policies: fileShare.PolicyInfo[] = [{ uri: record.uri, operationMode: record.operation_mode }];
      stage = 'restore_check';
      const checks = await fileShare.checkPersistentPermission(policies);
      record.persisted = checks.length === 1 && checks[0];
      record.checked_modes = [1, 2, 3, 4, 8, 16, 31];
      record.checked_results = await fileShare.checkPersistentPermission(record.checked_modes.map(
        (value: number): fileShare.PolicyInfo => { return { uri: record.uri, operationMode: value }; }));
      if (!record.persisted) throw new Error('Persistent policy unavailable');
      stage = 'restore_activate';
      await fileShare.activatePermission(policies);
      record.activated = true;
      record.error = undefined;
      stage = 'restore_record';
      ModelStorageService.save(context, record);
      ModelStorageService.status('policy_activated');
      hilog.info(0, 'OllamaStorage', 'stored policy activated; filesystem operations unverified');
    } catch (error) { ModelStorageService.failure(context, record, stage, error as BusinessError, replace); }
  }
}
