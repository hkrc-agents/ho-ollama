import { StatusBarViewExtensionAbility } from '@kit.DeskTopExtensionKit';
import { Want, UIExtensionContentSession } from '@kit.AbilityKit';
import { hilog } from '@kit.PerformanceAnalysisKit';

export default class TrayViewAbility extends StatusBarViewExtensionAbility {
  onSessionCreate(_want: Want, session: UIExtensionContentSession): void {
    try { session.loadContent('pages/TrayMenu'); } catch (error) {
      hilog.error(0, 'OllamaTray', 'menu %{public}s', String(error));
    }
  }
}
