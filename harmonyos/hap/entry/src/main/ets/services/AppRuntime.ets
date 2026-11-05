import { common } from '@kit.AbilityKit';
import preferences from '@ohos.data.preferences';
import { backgroundTaskManager } from '@kit.BackgroundTasksKit';
import wantAgent from '@ohos.app.ability.wantAgent';
import util from '@ohos.util';
import { http } from '@kit.NetworkKit';
import bridge from 'libollamabridge.so';
import { OllamaApi } from './OllamaApi';
import { ModelStorageService } from './ModelStorageService';
import fs from '@ohos.file.fs';
import certManager from '@ohos.security.certManager';

export class AppRuntime {
  static context: common.UIAbilityContext;
  static port: number = 11434;
  static modelsDir: string = '';
  static task: boolean = false;
  static store: preferences.Preferences;
  static webBridgeScript: string = '';
  private static starting: Promise<void> | undefined = undefined;
  private static observerTimer: number = -1;
  private static observing: boolean = false;
  private static desired: boolean = false;
  private static lastToken: string = '';
  private static generation: number = 0;
  private static systemCaDirectory: string = '';
  private static prepareSystemCaDirectory(): void {
    let status: string = 'unavailable';
    let detail: string = '';
    AppRuntime.systemCaDirectory = '';
    try {
      const path = certManager.getCertificateStorePath({ certType: certManager.CertType.CA_CERT_SYSTEM });
      if (!path.startsWith('/') || !fs.statSync(path).isDirectory()) throw new Error('Invalid system CA directory');
      AppRuntime.systemCaDirectory = path;
      status = 'available';
    } catch (error) { detail = String(error); }
    // Availability is not a claim that an HTTPS request has succeeded. An
    // unavailable provider must not prevent local/offline CPU inference.
    const record = fs.openSync(AppRuntime.context.filesDir + '/tls-store-status.json',
      fs.OpenMode.CREATE | fs.OpenMode.WRITE_ONLY | fs.OpenMode.TRUNC);
    try { fs.writeSync(record.fd, JSON.stringify({ status, path: AppRuntime.systemCaDirectory,
      source: 'CertificateManager.CA_CERT_SYSTEM', detail })); }
    finally { fs.closeSync(record); }
  }
  private static recordObserver(state: string): void {
    try {
      const file = fs.openSync(AppRuntime.context.filesDir + '/app-runtime-observer.json', fs.OpenMode.CREATE | fs.OpenMode.READ_WRITE | fs.OpenMode.TRUNC);
      try { fs.writeSync(file.fd, JSON.stringify({ generation: AppRuntime.generation, state, time: Date.now() })); }
      finally { fs.closeSync(file); }
    } catch (_) {}
  }
  private static async observe(): Promise<void> {
    if (!AppRuntime.desired || AppRuntime.starting || AppRuntime.observing) return;
    AppRuntime.observing = true;
    try {
      const token = bridge.appToken(AppRuntime.context.filesDir);
      if (token.length !== 64) throw new Error('APP instance unavailable');
      if (token === AppRuntime.lastToken && AppStorage.get<string>('serviceStatus') === 'ready') return;
      const request = http.createHttp();
      try {
        const reply = await request.request(`http://127.0.0.1:${AppRuntime.appPort()}/api/harmony/services/logs`, {
          method: http.RequestMethod.GET, header: { Cookie: 'token=' + token }, connectTimeout: 1000, readTimeout: 1000
        });
        if (reply.responseCode !== 200) throw new Error('APP recovery not authenticated');
      } finally { request.destroy(); }
      if (!AppRuntime.desired) return;
      AppRuntime.lastToken = token;
      AppStorage.setOrCreate('serviceStatus', 'ready');
      AppRuntime.generation++;
      AppStorage.setOrCreate('appRuntimeGeneration', AppRuntime.generation);
      AppRuntime.recordObserver('ready');
    } catch (_) {
      if (AppRuntime.desired) {
        AppStorage.setOrCreate('serviceStatus', 'recovering');
        AppRuntime.recordObserver('recovering');
      }
    } finally { AppRuntime.observing = false; }
  }
  static async initialize(context: common.UIAbilityContext): Promise<void> {
    AppRuntime.context = context;
    await ModelStorageService.restore(context);
    AppRuntime.prepareSystemCaDirectory();
    AppRuntime.webBridgeScript = new util.TextDecoder().decodeToString(
      await context.resourceManager.getRawFileContent('harmony-bridge.js'));
    AppRuntime.store = await preferences.getPreferences(context, 'ollama-settings');
    AppRuntime.port = Number(await AppRuntime.store.get('port', 11434));
    AppRuntime.modelsDir = context.filesDir + '/models';
    AppStorage.setOrCreate('serviceStatus', bridge.status(context.filesDir));
    AppStorage.setOrCreate('appRuntimeGeneration', AppRuntime.generation);
    if (AppRuntime.observerTimer < 0) AppRuntime.observerTimer = setInterval(() => { AppRuntime.observe().catch(() => {}); }, 1000);
  }
  static api(): OllamaApi { return new OllamaApi(AppRuntime.port); }
  static async save(port: number): Promise<void> {
    if (!Number.isInteger(port) || port < 1024 || port > 65535) throw new Error('端口范围为 1024–65535');
    if (AppRuntime.desired || AppRuntime.starting || bridge.appToken(AppRuntime.context.filesDir).length > 0 ||
      bridge.status(AppRuntime.context.filesDir).startsWith('running')) throw new Error('请先停止服务再保存端口');
    await AppRuntime.store.put('port', port);
    await AppRuntime.store.flush();
    AppRuntime.port = port;
  }
  static async start(): Promise<void> {
    if (AppRuntime.starting) return AppRuntime.starting;
    const operation = AppRuntime.startServices();
    AppRuntime.starting = operation;
    try { await operation; } finally { AppRuntime.starting = undefined; }
  }
  static appPort(): number { return AppRuntime.port === 11435 ? 11437 : 11435; }
  private static async startServices(): Promise<void> {
    AppRuntime.desired = true;
    AppStorage.setOrCreate('serviceStatus', 'starting');
    try {
      if (!AppRuntime.task) {
        const agent = await wantAgent.getWantAgent({
          wants: [{ bundleName: 'com.pico.ollama', abilityName: 'EntryAbility' }],
          operationType: wantAgent.OperationType.START_ABILITY, requestCode: 1,
          wantAgentFlags: [wantAgent.WantAgentFlags.UPDATE_PRESENT_FLAG]
        });
        await backgroundTaskManager.startBackgroundRunning(AppRuntime.context,
          backgroundTaskManager.BackgroundMode.TASK_KEEPING, agent);
        AppRuntime.task = true;
      }
      const result = bridge.appStart(AppRuntime.context.filesDir, AppRuntime.port, AppRuntime.systemCaDirectory);
      if (result.startsWith('error:')) throw new Error(result);
      // APP readiness is independent of core readiness: users must still be
      // able to view errors/configuration and restart a stopped/conflicting core.
      for (let attempt = 0; attempt < 60; attempt++) {
        try {
          const token = bridge.appToken(AppRuntime.context.filesDir);
          if (token.length !== 64) {
            throw new Error('官方 APP 服务尚未就绪');
          }
          const request = http.createHttp();
          try {
            const response = await request.request(`http://127.0.0.1:${AppRuntime.appPort()}/api/harmony/services`, {
              method: http.RequestMethod.GET, header: { Cookie: 'token=' + token },
              connectTimeout: 1000, readTimeout: 1000
            });
            if (response.responseCode !== 200) throw new Error('APP HTTP 尚未就绪');
          } finally { request.destroy(); }
          AppStorage.setOrCreate('serviceStatus', 'ready');
          AppRuntime.lastToken = token;
          AppRuntime.recordObserver('ready');
          return;
        } catch (_) { await new Promise<void>((resolve) => { setTimeout(resolve, 300); }); }
      }
      throw new Error('服务未就绪，请查看日志');
    } catch (error) {
      AppStorage.setOrCreate('serviceStatus', `failed: ${String(error)}`);
      throw new Error(String(error));
    }
  }
  static async stop(): Promise<void> {
    AppRuntime.desired = false;
    if (AppRuntime.starting) { try { await AppRuntime.starting; } catch (_) {} }
    const appResult = bridge.appStop(AppRuntime.context.filesDir);
    if (appResult.startsWith('error:')) throw new Error(appResult);
    const result = bridge.stop(AppRuntime.context.filesDir);
    if (result.startsWith('error:')) throw new Error(result);
    AppStorage.setOrCreate('serviceStatus', 'stopping');
    for (let attempt = 0; attempt < 200; attempt++) {
      const shutdown = bridge.appShutdownStatus(AppRuntime.context.filesDir);
      if (shutdown.startsWith('error:')) {
        AppStorage.setOrCreate('serviceStatus', shutdown);
        throw new Error(shutdown);
      }
      if (bridge.status(AppRuntime.context.filesDir) === 'stopped' &&
        shutdown === 'stopped') {
        if (AppRuntime.task) {
          await backgroundTaskManager.stopBackgroundRunning(AppRuntime.context);
          AppRuntime.task = false;
        }
        AppStorage.setOrCreate('serviceStatus', 'stopped');
        return;
      }
      await new Promise<void>((resolve) => { setTimeout(resolve, 100); });
    }
    throw new Error('服务尚未退出');
  }
}
