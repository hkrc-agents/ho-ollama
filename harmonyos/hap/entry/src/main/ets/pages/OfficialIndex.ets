import { webview } from '@kit.ArkWeb';
import { AppRuntime } from '../services/AppRuntime';
import bridge from 'libollamabridge.so';
import { picker } from '@kit.CoreFileKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { promptAction } from '@kit.ArkUI';
import { DesktopLogs } from '../components/DesktopLogs';
import { LoginStartupSettings } from '../components/LoginStartupSettings';
import { LoginStartupService } from '../services/LoginStartupService';
import { NotificationSettingsService } from '../services/NotificationSettingsService';
import { ModelStorageService } from '../services/ModelStorageService';
import { ModelTransferService, ModelTransferState } from '../services/ModelTransferService';
import { NavigationIntent } from '../services/NavigationIntent';

// Native shell only. Chat/settings/storage are the upstream React + Go APP.
@Entry
@Component
struct OfficialIndex {
  private controller: webview.WebviewController = new webview.WebviewController();
  @State error: string = '';
  @State ready: boolean = false;
  @State logsVisible: boolean = false;
  @State startupSettingsVisible: boolean = false;
  @StorageLink('trayDestination') @Watch('onTrayDestination') trayDestination: string = '';
  @StorageLink('serviceStatus') @Watch('onServiceChanged') status: string = 'stopped';
  @StorageLink('appRuntimeGeneration') @Watch('onAppGeneration') generation: number = 0;
  private attached: boolean = false;
  private navigation: NavigationIntent = new NavigationIntent();
  private transferStatus(state: ModelTransferState): void {
    if (!this.controller.getUrl().startsWith(`http://127.0.0.1:${AppRuntime.appPort()}/`)) return;
    this.controller.runJavaScript('window.dispatchEvent(new CustomEvent("harmony-model-transfer",{detail:' + JSON.stringify(state) + '}));').catch(() => {});
  }
  private async refreshLoginStartup(): Promise<void> {
    const origin = `http://127.0.0.1:${AppRuntime.appPort()}/`;
    if (!this.controller.getUrl().startsWith(origin)) return;
    const status = await LoginStartupService.refresh(AppRuntime.context);
    if (!this.controller.getUrl().startsWith(origin)) return;
    // Only a finite public status enum is published; never tokens, paths or a
    // general-purpose native API. The web page has no setter for the OS state.
    const script = 'window.dispatchEvent(new CustomEvent("harmony-login-startup-state", {detail:' +
      JSON.stringify({ status }) + '}));';
    this.controller.runJavaScript(script).catch((error: Error) => {
      hilog.warn(0, 'OllamaStartup', 'status delivery %{public}s', String(error));
    });
  }
  private async refreshNotificationSettings(): Promise<void> {
    const origin = `http://127.0.0.1:${AppRuntime.appPort()}/`;
    if (!this.controller.getUrl().startsWith(origin)) return;
    const state = await NotificationSettingsService.refresh(AppRuntime.context);
    if (!this.controller.getUrl().startsWith(origin)) return;
    this.controller.runJavaScript('window.dispatchEvent(new CustomEvent("harmony-notification-state",{detail:' +
      JSON.stringify(state) + '}));').catch(() => {});
  }
  private authenticate(preserveLocation: boolean = false): void {
    const token = bridge.appToken(AppRuntime.context.filesDir);
    if (token.length !== 64) throw new Error('官方 APP 认证未就绪');
    const origin = `http://127.0.0.1:${AppRuntime.appPort()}`;
    webview.WebCookieManager.configCookieSync(origin, `token=${token}; Path=/; HttpOnly; SameSite=Strict`, false, true);
    const current = this.controller.getUrl();
    this.navigation.update(this.trayDestination);
    this.controller.loadUrl(this.navigation.location(origin, current, preserveLocation));
    this.ready = true;
    this.logsVisible = this.trayDestination.startsWith('logs:');
  }
  private onTrayDestination(): void {
    this.navigation.update(this.trayDestination);
    this.logsVisible = this.trayDestination.startsWith('logs:');
    if (this.ready && this.trayDestination.startsWith('settings:')) {
      this.controller.loadUrl(`http://127.0.0.1:${AppRuntime.appPort()}/settings`);
    }
  }
  private onServiceChanged(): void {
    if (!this.attached) return;
    if (this.status === 'ready') {
      try { this.authenticate(true); this.error = ''; } catch (error) { this.error = String(error); }
    } else if (this.status === 'recovering') {
      this.ready = false;
      this.error = 'APP 服务正在恢复，可稍候或重试';
    } else if (this.status === 'stopped') {
      this.ready = false;
      this.error = '服务已停止';
      this.controller.loadUrl('about:blank');
    }
  }
  private onAppGeneration(): void {
    if (this.attached && this.ready && this.status === 'ready') {
      try { this.authenticate(true); this.error = ''; } catch (error) { this.error = String(error); }
    }
  }
  private async connect(): Promise<void> {
    try {
      await AppRuntime.start();
      // A service-ready observer can already have committed a tray request
      // while start() was resolving. Do not replace that page with Chat.
      this.authenticate(true);
    } catch (error) { this.error = String(error); }
  }
  build() {
    Stack({ alignContent: Alignment.TopStart }) {
      Web({ src: 'about:blank', controller: this.controller })
        .javaScriptAccess(true)
        .domStorageAccess(true)
        .fileAccess(false)
        .javaScriptOnDocumentStart([{ script: AppRuntime.webBridgeScript, scriptRules: ['*'] }])
        .onShowFileSelector((event) => {
          if (!event) return false;
          const origin = `http://127.0.0.1:${AppRuntime.appPort()}/`;
          if (!this.controller.getUrl().startsWith(origin)) {
            event.result.handleFileList([]); return true;
          }
          const options = new picker.DocumentSelectOptions();
          options.maxSelectNumber = event.fileSelector.getMode() === FileSelectorMode.FileOpenMultipleMode ? 10 : 1;
          const dialog = new picker.DocumentViewPicker(AppRuntime.context);
          dialog.select(options).then((uris: string[]) => {
            event.result.handleFileList(uris);
          }).catch((error: Error) => {
            hilog.error(0, 'OllamaApp', 'file picker %{public}s', String(error));
            promptAction.showToast({ message: '无法打开文件选择器：' + String(error) });
            event.result.handleFileList([]);
          });
          return true;
        })
        .onControllerAttached(() => {
          this.attached = true;
          if (AppStorage.get<boolean>('autoStartOllama') !== false) this.connect();
          else this.error = '服务已停止';
        })
        .onPageEnd((event) => {
          if (!event || !this.attached || !this.ready || event.url !== this.controller.getUrl()) return;
          this.navigation.update(this.trayDestination);
          const target = this.navigation.committed(`http://127.0.0.1:${AppRuntime.appPort()}`, event.url);
          if (target.length > 0) {
            try { this.controller.loadUrl(target); } catch (error) { this.error = String(error); }
          }
        })
        .onLoadIntercept((event) => {
          const url = event?.data.getRequestUrl() || '';
          const trusted = this.controller.getUrl().startsWith(`http://127.0.0.1:${AppRuntime.appPort()}/`);
          if (trusted && url.startsWith('harmony-ollama://export-gguf?model=')) {
            const model = decodeURIComponent(url.substring('harmony-ollama://export-gguf?model='.length));
            ModelTransferService.export(AppRuntime.context, AppRuntime.appPort(), bridge.appToken(AppRuntime.context.filesDir), model,
              (state: ModelTransferState) => this.transferStatus(state)).catch((error: Error) => {
                promptAction.showToast({ message: String(error) });
              });
            return true;
          }
          if (trusted && url === 'harmony-ollama://cancel-model-export') { ModelTransferService.cancel(); return true; }
          if (trusted && url === 'harmony-ollama://model-transfer-status') {
            try { const state = ModelTransferService.restore(AppRuntime.context); if (state) this.transferStatus(state); }
            catch (error) { promptAction.showToast({ message: String(error) }); }
            return true;
          }
          if (trusted && url === 'harmony-ollama://model-directory') { ModelStorageService.select(AppRuntime.context); return true; }
          if (trusted && url === 'harmony-ollama://notification-status') {
            this.refreshNotificationSettings().catch(() => {});
            return true;
          }
          if (trusted && url === 'harmony-ollama://notification-settings') {
            NotificationSettingsService.open(AppRuntime.context).then(() => this.refreshNotificationSettings())
              .catch((error: Error) => { promptAction.showToast({ message: '无法打开系统通知设置：' + String(error) }); });
            return true;
          }
          if (url === 'harmony-ollama://login-startup-status' &&
            this.controller.getUrl().startsWith(`http://127.0.0.1:${AppRuntime.appPort()}/`)) {
            this.refreshLoginStartup().catch((error: Error) => {
              hilog.warn(0, 'OllamaStartup', 'refresh %{public}s', String(error));
            });
            return true;
          }
          if (url === 'harmony-ollama://login-startup' &&
            this.controller.getUrl().startsWith(`http://127.0.0.1:${AppRuntime.appPort()}/`)) {
            LoginStartupService.openSettings(AppRuntime.context).catch((error: Error) => {
              hilog.warn(0, 'OllamaStartup', 'settings navigation %{public}s', String(error));
              this.startupSettingsVisible = true;
            });
            return true;
          }
          return url !== 'about:blank' && !url.startsWith(`http://127.0.0.1:${AppRuntime.appPort()}/`);
        })
        .width('100%').height('100%')
      if (!this.ready) {
        Column({ space: 16 }) {
          Text('Ollama').fontSize(28)
          Text(this.error || '正在启动官方 Ollama APP…')
          if (this.error) { Button('重试').onClick(() => { this.error = ''; this.connect(); }); }
        }.padding(32)
      }
      if (this.logsVisible) { DesktopLogs({ visible: $logsVisible }) }
      if (this.startupSettingsVisible) { LoginStartupSettings({ visible: $startupSettingsVisible }) }
    }.width('100%').height('100%')
  }
}
