import { common } from '@kit.AbilityKit';
import { notificationManager } from '@kit.NotificationKit';
import { BusinessError } from '@kit.BasicServicesKit';
import { fileIo as fs } from '@kit.CoreFileKit';

export class NotificationSettingsState {
  notifications: string = 'unknown';
  serviceBanners: string = 'unknown';
  code: number = 0;
  slotCode: number = 0;
  level: number = -1;
}

export class NotificationSettingsService {
  static async open(context: common.UIAbilityContext): Promise<void> {
    await notificationManager.openNotificationSettings(context);
  }
  static async refresh(context: common.UIAbilityContext): Promise<NotificationSettingsState> {
    const state = new NotificationSettingsState();
    try {
      state.notifications = await notificationManager.isNotificationEnabled() ? 'enabled' : 'disabled';
    } catch (error) {
      state.code = (error as BusinessError).code;
      state.notifications = state.code === 801 ? 'unsupported' : 'error';
    }
    try {
      const slot = await notificationManager.getSlot(notificationManager.SlotType.SERVICE_INFORMATION);
      // These APIs expose the service channel level, NOT the separate global
      // banner switch or DND. The UI must state this distinction explicitly.
      const level = slot.notificationLevel ?? slot.level;
      if (typeof level === 'number') {
        state.level = level;
        state.serviceBanners = slot.enabled === false || level < notificationManager.SlotLevel.LEVEL_HIGH ? 'disabled' : 'enabled';
      }
    } catch (error) {
      state.slotCode = (error as BusinessError).code;
      state.serviceBanners = state.slotCode === 801 ? 'unsupported' : 'unknown';
    }
    try {
      const file = fs.openSync(context.filesDir + '/notification-settings-status.json',
        fs.OpenMode.WRITE_ONLY | fs.OpenMode.CREATE | fs.OpenMode.TRUNC);
      try { fs.writeSync(file.fd, JSON.stringify(state)); } finally { fs.closeSync(file); }
    } catch (_) {}
    return state;
  }
}
