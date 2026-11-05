import { LoginStartupService } from '../services/LoginStartupService';
import { AppRuntime } from '../services/AppRuntime';
import { hilog } from '@kit.PerformanceAnalysisKit';

@Component
export struct LoginStartupSettings {
  @Link visible: boolean;
  @StorageLink('loginStartupStatus') status: string = 'unknown';
  @State navigationError: string = '';
  aboutToAppear(): void { LoginStartupService.refresh(AppRuntime.context); }
  build() {
    Column({ space: 16 }) {
      Row() {
        Text('登录自启动').fontSize(22).layoutWeight(1)
        Button('关闭').onClick(() => { this.visible = false; })
      }.width('100%')
      Text('系统设置状态：' + (this.status === 'enabled' ? '已开启' : this.status === 'disabled' ? '未开启' :
        this.status === 'unsupported' ? '当前系统不支持' : this.status === 'error' ? '查询失败，请重试' : '查询中…')).fontSize(18)
      Text('系统设置 → 应用和服务 → 启动管理 → 添加应用，选择 Ollama 后点击完成。英文系统路径：Settings → Apps & services → Launch manager → Add app。').fontSize(16)
      Text('由系统管理登录自启动；修改后返回设置页会重新查询状态。').fontSize(14)
      Button('打开应用启动管理').onClick(() => {
        LoginStartupService.openSettings(AppRuntime.context).catch((error: Error) => {
          this.navigationError = '无法打开系统设置，请按上述路径手工进入。';
          hilog.warn(0, 'OllamaStartup', 'settings navigation %{public}s', String(error));
        });
      })
      if (this.navigationError) { Text(this.navigationError).fontSize(14) }
      Button('刷新系统状态').onClick(() => { LoginStartupService.refresh(AppRuntime.context); })
    }.padding(24).width('100%').height('100%').backgroundColor($r('sys.color.ohos_id_color_background'))
  }
}
