import { common } from '@kit.AbilityKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { fileIo as fs } from '@kit.CoreFileKit';
import { statusBarManager } from '@kit.DeskTopExtensionKit';

@Entry
@Component
struct TrayMenu {
  private lastHeight: number = 168;
  private resize(value: number): void {
    const height = Math.ceil(value);
    if (!Number.isFinite(height) || height < 64 || height > 512 || height === this.lastHeight) return;
    try {
      const context = this.getUIContext().getHostContext() as common.UIExtensionContext;
      statusBarManager.updateQuickOperationHeight(context, height);
      this.lastHeight = height;
    } catch (error) { hilog.warn(0, 'OllamaTray', 'menu sizing %{public}s', String(error)); }
  }
  private open(action: string): void {
    const context = this.getUIContext().getHostContext() as common.UIExtensionContext;
    context.startAbility({ bundleName: 'com.pico.ollama', abilityName: 'MainAbility',
      parameters: { action: 'tray-' + action } }).catch((error: Error) => {
      try {
        const file = fs.openSync(context.filesDir + '/tray-menu-error.json', fs.OpenMode.READ_WRITE | fs.OpenMode.CREATE | fs.OpenMode.TRUNC);
        try { fs.writeSync(file.fd, JSON.stringify({ action, message: String(error) })); }
        finally { fs.closeSync(file); }
      } catch (_) {}
      hilog.error(0, 'OllamaTray', 'action %{public}s', String(error));
    });
  }
  build() {
    Menu() {
      MenuItemGroup() {
        MenuItem({ content: 'Open Ollama' }).contentFont({ size: 18 })
          .padding({ left: 18, right: 18 }).onClick(() => this.open('show'))
        MenuItem({ content: 'Settings' }).contentFont({ size: 18 })
          .padding({ left: 18, right: 18 }).onClick(() => this.open('settings'))
        MenuItem({ content: 'View logs' }).contentFont({ size: 18 })
          .padding({ left: 18, right: 18 }).onClick(() => this.open('logs'))
      }
      MenuItemGroup() {
        MenuItem({ content: 'Quit Ollama' }).contentFont({ size: 18 })
          .padding({ left: 18, right: 18 }).onClick(() => this.open('quit'))
      }
    }.width('100%').onAreaChange((_oldArea: Area, area: Area) => this.resize(Number(area.height)))
  }
}
