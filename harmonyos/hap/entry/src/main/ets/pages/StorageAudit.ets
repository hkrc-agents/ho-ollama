import { common } from '@kit.AbilityKit';
import { ModelStorageService } from '../services/ModelStorageService';

// Native diagnostic page: UI event vs HDC Want, and ArkTS vs public NDK.
// Never changes the model root, revokes permissions, or accepts a URI input.
@Entry
@Component
struct StorageAudit {
  @State busy: boolean = false;
  @State result: string = '';
  @StorageLink('modelStorageStatus') status: string = 'unknown';
  private async run(kind: string): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    const context = this.getUIContext().getHostContext() as common.UIAbilityContext;
    try {
      if (kind === 'native') this.result = await ModelStorageService.auditNative(context);
      else if (kind === 'js') { await ModelStorageService.retrySelected(context); this.result = 'ArkTS request finished; inspect native selection record.'; }
      else if (kind === 'authorize') { await ModelStorageService.authorizeSelected(context); this.result = 'System authorization finished; inspect selection record.'; }
    } catch (error) { this.result = String(error); }
    finally { this.busy = false; }
  }
  build() {
    Column({ space: 16 }) {
      Text('Storage permission diagnostics').fontSize(24)
      Text('Diagnostics only. Existing models remain in the private directory.').fontSize(16)
      Text('Policy status: ' + this.status).fontSize(16)
      Row({ space: 16 }) {
        Button('Retry ArkTS persistence').enabled(!this.busy).onClick(() => { this.run('js'); })
        Button('Audit public NDK permissions').enabled(!this.busy).onClick(() => { this.run('native'); })
        Button('Authorize selected folder').enabled(!this.busy).onClick(() => { this.run('authorize'); })
      }
      Scroll() { Text(this.result).fontSize(14).copyOption(CopyOptions.InApp).width('100%') }.layoutWeight(1).width('100%')
      Button('Return to Ollama').enabled(!this.busy).onClick(() => {
        AppStorage.setOrCreate('trayDestination', 'settings:' + Date.now().toString());
        this.getUIContext().getRouter().back();
      })
    }.padding(24).width('100%').height('100%')
  }
}
