import { common, wantAgent } from '@kit.AbilityKit';
import { notificationManager } from '@kit.NotificationKit';
import { hilog } from '@kit.PerformanceAnalysisKit';

// A window toast is not a background notification. Keep this notice separate
// from the long-running task and never make service availability depend on it.
export class BackgroundNotice {
  private static readonly id: number = 21021;
  private static asked: boolean = false;
  private static published: boolean = false;

  static async prepare(context: common.UIAbilityContext): Promise<void> {
    try {
      if (await notificationManager.isNotificationEnabled() || BackgroundNotice.asked) return;
      BackgroundNotice.asked = true;
      // Request while the main window is still visible, not after hideAbility.
      await notificationManager.requestEnableNotification(context);
    } catch (error) {
      hilog.warn(0, 'OllamaTray', 'notification authorization %{public}s', String(error));
    }
  }

  static async publish(context: common.UIAbilityContext): Promise<void> {
    try {
      if (!await notificationManager.isNotificationEnabled()) return;
      const agent = await wantAgent.getWantAgent({
        wants: [{ bundleName: context.abilityInfo.bundleName, moduleName: 'entry', abilityName: 'MainAbility',
          parameters: { action: 'tray-show' } }],
        operationType: wantAgent.OperationType.START_ABILITY, requestCode: BackgroundNotice.id,
        wantAgentFlags: [wantAgent.WantAgentFlags.UPDATE_PRESENT_FLAG]
      });
      await notificationManager.publish({
        id: BackgroundNotice.id,
        notificationSlotType: notificationManager.SlotType.SERVICE_INFORMATION,
        content: {
          notificationContentType: notificationManager.ContentType.NOTIFICATION_CONTENT_BASIC_TEXT,
          normal: { title: 'Ollama 已隐藏到托盘',
            text: '后台服务仍在运行。点击此通知打开窗口；彻底退出请选择托盘中的 Quit Ollama。' }
        },
        wantAgent: agent,
        tapDismissed: true
      });
      BackgroundNotice.published = true;
      hilog.info(0, 'OllamaTray', 'background notification published');
    } catch (error) {
      hilog.warn(0, 'OllamaTray', 'background notification %{public}s', String(error));
    }
  }

  static async clear(): Promise<void> {
    if (!BackgroundNotice.published) return;
    try {
      await notificationManager.cancel(BackgroundNotice.id);
      BackgroundNotice.published = false;
    } catch (error) {
      hilog.warn(0, 'OllamaTray', 'notification cancel %{public}s', String(error));
    }
  }
}
