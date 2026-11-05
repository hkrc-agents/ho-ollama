import { UIAbility, Want, AbilityConstant, contextConstant } from '@kit.AbilityKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { BusinessError } from '@kit.BasicServicesKit';
import { TrayService } from '../services/TrayService';
import { LoginStartupService } from '../services/LoginStartupService';

// Home/login/debug entry. The real window owner is attached to the tray.
export default class EntryAbility extends UIAbility {
  private want: Want | undefined = undefined;
  onCreate(want: Want, launchParam: AbilityConstant.LaunchParam): void {
    LoginStartupService.recordLaunch(this.context, launchParam);
    this.want = want;
  }
  onWindowStageCreate(): void { this.forward().catch((error: Error) => {
    hilog.error(0, 'OllamaApp', 'tray bootstrap %{public}s', String(error));
  }); }
  private async forward(): Promise<void> {
    await TrayService.install(this.context);
    if (!TrayService.installed) throw new Error('托盘注册失败，不能启动后台宿主');
    const target: Want = { bundleName: 'com.pico.ollama', moduleName: 'entry', abilityName: 'MainAbility',
      parameters: this.want?.parameters };
    try {
      await this.context.startAbility(target, {
        processMode: contextConstant.ProcessMode.ATTACH_TO_STATUS_BAR_ITEM,
        startupVisibility: contextConstant.StartupVisibility.STARTUP_SHOW
      });
    } catch (error) {
      const code = (error as BusinessError).code;
      if (code !== 16000068) throw new Error(String(error));
      await this.context.startAbility(target);
    }
    await this.context.terminateSelf();
  }
}
