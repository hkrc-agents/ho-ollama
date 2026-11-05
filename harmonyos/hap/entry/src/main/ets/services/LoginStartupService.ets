import { autoStartupManager, AbilityConstant, common } from '@kit.AbilityKit';
import { fileIo as fs } from '@kit.CoreFileKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { BusinessError } from '@kit.BasicServicesKit';

// System-owned setting. Never pretend that an application preference enables it.
export class LoginStartupService {
  // Explicit system Settings Want, not a browser App Link. No setting setter.
  static async openSettings(context: common.UIAbilityContext): Promise<void> {
    await context.startAbility({
      bundleName: 'com.huawei.hmos.settings',
      abilityName: 'com.huawei.hmos.settings.MainAbility',
      uri: 'pc_app_setup_settings'
    });
  }
  static status: string = 'unknown';
  static reason: number = -1;
  static recordLaunch(context: common.UIAbilityContext, param: AbilityConstant.LaunchParam): void {
    LoginStartupService.reason = param.launchReason;
    const text = JSON.stringify({ time: Date.now(), reason: param.launchReason,
      systemAutoStartup: param.launchReason === AbilityConstant.LaunchReason.AUTO_STARTUP });
    try {
      const file = fs.openSync(context.filesDir + '/login-startup-launch.json',
        fs.OpenMode.WRITE_ONLY | fs.OpenMode.CREATE | fs.OpenMode.TRUNC);
      try { fs.writeSync(file.fd, text); } finally { fs.closeSync(file); }
    } catch (error) { hilog.error(0, 'OllamaStartup', 'launch record %{public}s', String(error)); }
  }
  static async refresh(context: common.UIAbilityContext): Promise<string> {
    let code: number = 0;
    try {
      LoginStartupService.status = await autoStartupManager.getAutoStartupStatusForSelf() ? 'enabled' : 'disabled';
    } catch (error) {
      code = (error as BusinessError).code;
      LoginStartupService.status = code === 801 ? 'unsupported' : 'error';
      hilog.warn(0, 'OllamaStartup', 'query code %{public}d', code);
    }
    AppStorage.setOrCreate('loginStartupStatus', LoginStartupService.status);
    const text = JSON.stringify({ time: Date.now(), status: LoginStartupService.status,
      code, launchReason: LoginStartupService.reason, settingOwner: 'system' });
    try {
      const file = fs.openSync(context.filesDir + '/login-startup-status.json',
        fs.OpenMode.WRITE_ONLY | fs.OpenMode.CREATE | fs.OpenMode.TRUNC);
      try { fs.writeSync(file.fd, text); } finally { fs.closeSync(file); }
    } catch (error) { hilog.error(0, 'OllamaStartup', 'status record %{public}s', String(error)); }
    return LoginStartupService.status;
  }
}
