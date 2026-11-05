import { UIAbility, Want, AbilityConstant } from '@kit.AbilityKit';
import { window } from '@kit.ArkUI';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { AppRuntime } from '../services/AppRuntime';
import bridge from 'libollamabridge.so';
import { exportAttachmentFixture } from '../services/PickerFixture';
import { TrayService } from '../services/TrayService';
import { fileIo as fs } from '@kit.CoreFileKit';
import { LoginStartupService } from '../services/LoginStartupService';
import { ModelStorageService } from '../services/ModelStorageService';
import { BackgroundNotice } from '../services/BackgroundNotice';
import BuildProfile from 'BuildProfile';

export default class MainAbility extends UIAbility {
  private initialWant: Want | undefined = undefined;
  private initialized: boolean = false;
  private operations: Promise<void> = Promise.resolve();
  private stage: window.WindowStage | undefined = undefined;
  private quitting: boolean = false;
  private hiding: Promise<void> | undefined = undefined;
  private hidden: boolean = false;
  onCreate(want: Want, launchParam: AbilityConstant.LaunchParam): void {
    this.initialWant = want;
    AppStorage.setOrCreate('autoStartOllama', want.parameters?.['action'] !== 'stop');
  }
  onForeground(): void { LoginStartupService.refresh(this.context); }
  onWindowStageCreate(stage: window.WindowStage): void {
    this.stage = stage;
    AppRuntime.initialize(this.context).then(async () => {
      this.initialized = true;
      await stage.loadContent('pages/OfficialIndex');
      TrayService.watch(stage);
      await TrayService.install(this.context);
      stage.on('windowStageClose', (): boolean => {
        if (this.quitting) return false;
        this.hideToTray('windowStageClose').catch(() => {});
        return true;
      });
      if (this.initialWant) await this.dispatch(this.initialWant);
    }).catch((error: Error) => { hilog.error(0, 'OllamaApp', 'init %{public}s', String(error)); });
  }
  async onPrepareToTerminateAsync(): Promise<boolean> {
    if (this.quitting) return false;
    try { await this.hideToTray('prepareToTerminate'); } catch (_) {}
    return true;
  }
  private async hideToTray(reason: string): Promise<void> {
    if (this.quitting || this.hidden) return;
    if (this.hiding) return this.hiding;
    const operation = (async (): Promise<void> => {
      try {
        if (!TrayService.installed || !AppRuntime.task) throw new Error('托盘或后台任务尚未就绪，窗口暂不关闭');
        await BackgroundNotice.prepare(this.context);
        await this.context.hideAbility();
        this.hidden = true;
        this.recordLifecycle('hidden', reason);
        await BackgroundNotice.publish(this.context);
      } catch (error) {
        this.recordLifecycle('hide-failed', String(error));
        hilog.error(0, 'OllamaTray', 'hide %{public}s', String(error));
        throw new Error(String(error));
      }
    })();
    this.hiding = operation;
    try { await operation; } finally { this.hiding = undefined; }
  }
  private async showWindow(): Promise<void> {
    if (!this.stage) return;
    await TrayService.show(this.stage, this.context);
    this.hidden = false;
    await BackgroundNotice.clear();
    this.recordLifecycle('visible', 'restore');
  }
  private recordLifecycle(state: string, reason: string): void {
    try {
      const file = fs.openSync(this.context.filesDir + '/window-lifecycle.json', fs.OpenMode.READ_WRITE | fs.OpenMode.CREATE | fs.OpenMode.TRUNC);
      try { fs.writeSync(file.fd, JSON.stringify({ state, reason, task: AppRuntime.task, time: Date.now() })); }
      finally { fs.closeSync(file); }
    } catch (_) {}
  }
  onNewWant(want: Want): void {
    if (this.initialized) {
      this.operations = this.operations.then(() => this.dispatch(want)).catch((error: Error) => {
        hilog.error(0, 'OllamaApp', 'action %{public}s', String(error));
      });
    } else { this.initialWant = want; }
  }
  private async dispatch(want: Want): Promise<void> {
    const action = want.parameters?.['action'];
    // External debug Wants must not turn a release application into a test
    // command runner or expose fixture/export/permission-audit UI routes.
    // Normal UI calls, public inference APIs and tray actions are unchanged.
    if (!BuildProfile.DEBUG && typeof action === 'string' &&
      (action === 'command' || action === 'export-attachment-fixture' || action.startsWith('storage-') || action.startsWith('diagnostic-'))) {
      hilog.warn(0, 'OllamaApp', 'Diagnostic Want rejected in release');
      return;
    }
    if (typeof action === 'string' && action.startsWith('tray-')) await this.trayAction(action.substring(5));
    else if (action === 'start') await AppRuntime.start();
    else if (action === 'stop') await AppRuntime.stop();
    else if (action === 'diagnostic-app-crash') {
      const result = bridge.appDebugCrash(this.context.filesDir);
      hilog.info(0, 'OllamaApp', '%{public}s', result);
    }
    else if (action === 'export-attachment-fixture') await exportAttachmentFixture(this.context);
    // Authorization-first E4 probe. Never accepts a filesystem path in Want,
    // and never changes the active model library from a picker result alone.
    else if (action === 'storage-pick') await ModelStorageService.select(this.context);
    else if (action === 'storage-pick-rw') await ModelStorageService.selectReadWrite(this.context);
    else if (action === 'storage-authorize-selected') await ModelStorageService.authorizeSelected(this.context);
    else if (action === 'storage-audit-ui' && this.stage) {
      // Preserve the authenticated ArkWeb instance; replacing stage content
      // destroyed it and left about:blank on return in the first diagnostic.
      await this.stage.getMainWindowSync().getUIContext().getRouter().pushUrl({ url: 'pages/StorageAudit' });
    }
    else if (action === 'storage-audit-close' && this.stage) {
      AppStorage.setOrCreate('trayDestination', 'settings:' + Date.now().toString());
      this.stage.getMainWindowSync().getUIContext().getRouter().back();
    }
    else if (action === 'command') {
      const argv = want.parameters?.['argv'];
      if (typeof argv !== 'string') throw new Error('argv must be a JSON string array');
      const args = JSON.parse(argv) as string[];
      hilog.info(0, 'OllamaApp', '%{public}s', bridge.command(this.context.filesDir, args, AppRuntime.port));
    }
    else if (this.stage) {
      AppStorage.setOrCreate('trayDestination', 'show:' + Date.now().toString());
      await this.showWindow();
    }
  }
  private async trayAction(code: string): Promise<void> {
    try {
      const file = fs.openSync(this.context.filesDir + '/tray-action.json', fs.OpenMode.READ_WRITE | fs.OpenMode.CREATE | fs.OpenMode.TRUNC);
      try { fs.writeSync(file.fd, JSON.stringify({ code, received: Date.now() })); }
      finally { fs.closeSync(file); }
    } catch (_) {} // Diagnostics must never prevent restore or shutdown.
    if (code === 'show' && this.stage) {
      AppStorage.setOrCreate('trayDestination', 'show:' + Date.now().toString());
      await this.showWindow();
    }
    else if ((code === 'settings' || code === 'logs') && this.stage) {
      AppStorage.setOrCreate('trayDestination', code + ':' + Date.now().toString());
      await this.showWindow();
    }
    else if (code === 'stop') await AppRuntime.stop();
    else if (code === 'quit') {
      if (this.quitting) return;
      this.quitting = true;
      try {
        await this.showWindow();
        await AppRuntime.stop();
        await TrayService.remove(this.context);
        this.recordLifecycle('quit', 'tray');
        await this.context.terminateSelf();
      } catch (error) {
        this.quitting = false;
        this.recordLifecycle('quit-failed', String(error));
        throw new Error(String(error));
      }
    }
  }
}
