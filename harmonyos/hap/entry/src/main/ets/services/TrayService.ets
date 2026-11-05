import { common } from '@kit.AbilityKit';
import { window } from '@kit.ArkUI';
import { statusBarManager } from '@kit.DeskTopExtensionKit';
import { image } from '@kit.ImageKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { fileIo as fs } from '@kit.CoreFileKit';

export class TrayService {
  static installed: boolean = false;
  private static maps: image.PixelMap[] = [];
  private static status: window.WindowStatusType = window.WindowStatusType.UNDEFINED;
  private static record(context: common.UIAbilityContext, status: string): void {
    const file = fs.openSync(context.filesDir + '/tray-status.json', fs.OpenMode.READ_WRITE | fs.OpenMode.CREATE | fs.OpenMode.TRUNC);
    try { fs.writeSync(file.fd, JSON.stringify({ status, installed: TrayService.installed })); }
    finally { fs.closeSync(file); }
  }
  static watch(stage: window.WindowStage): void {
    stage.getMainWindowSync().on('windowStatusChange', (status: window.WindowStatusType) => {
      TrayService.status = status;
    });
  }
  static async install(context: common.UIAbilityContext): Promise<void> {
    if (TrayService.installed) return;
    try {
      TrayService.record(context, 'decoding icons');
      for (const name of ['tray-white.svg', 'tray-black.svg']) {
        const bytes = await context.resourceManager.getRawFileContent(name);
        const source = image.createImageSource(bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength));
        try { TrayService.maps.push(await source.createPixelMap({ desiredSize: { width: 48, height: 48 } })); }
        finally { source.release(); }
      }
      TrayService.record(context, 'registering');
      await new Promise<void>((resolve, reject) => {
        statusBarManager.addToStatusBar(context, {
          icons: { white: TrayService.maps[0], black: TrayService.maps[1] },
          // Four native 40-vp rows plus the measured Menu padding. The extension
          // reports its actual intrinsic height when fonts/content change.
          quickOperation: { title: 'Ollama for HarmonyOS', height: 168, abilityName: 'TrayViewAbility', moduleName: 'entry' },
          hoverTips: 'Ollama'
        }, (error) => { if (error) reject(error); else resolve(); });
      });
      TrayService.installed = true;
      TrayService.record(context, 'registered');
      hilog.info(0, 'OllamaTray', 'registered');
    } catch (error) {
      hilog.error(0, 'OllamaTray', 'unavailable: %{public}s', String(error));
      for (const map of TrayService.maps) map.release();
      TrayService.maps = [];
      TrayService.record(context, 'unavailable: ' + String(error));
    }
  }
  static async remove(context: common.UIAbilityContext): Promise<void> {
    if (!TrayService.installed) return;
    await new Promise<void>((resolve, reject) => {
      statusBarManager.removeFromStatusBar(context, (error) => { if (error) reject(error); else resolve(); });
    });
    TrayService.installed = false;
    for (const map of TrayService.maps) map.release();
    TrayService.maps = [];
    TrayService.record(context, 'removed');
  }
  static async show(stage: window.WindowStage, context: common.UIAbilityContext): Promise<void> {
    await context.showAbility();
    if (TrayService.status === window.WindowStatusType.MINIMIZE) await stage.getMainWindowSync().restore();
    await stage.getMainWindowSync().show();
  }
}
