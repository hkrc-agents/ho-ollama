import { fileIo as fs } from '@kit.CoreFileKit';
import { util } from '@kit.ArkTS';
import { AppRuntime } from '../services/AppRuntime';

// Platform-only equivalent of Windows' "View logs" folder action. Never reads
// arbitrary paths, authentication files, or an unbounded log into the UI.
@Component
export struct DesktopLogs {
  @Link visible: boolean;
  @State private contents: string = '';
  private tail(name: string): string {
    let file: fs.File | undefined = undefined;
    try {
      file = fs.openSync(AppRuntime.context.filesDir + '/' + name, fs.OpenMode.READ_ONLY);
      const size = fs.statSync(file.fd).size;
      const length = Math.min(size, 65536);
      const buffer = new ArrayBuffer(length);
      const count = fs.readSync(file.fd, buffer, { offset: Math.max(0, size - length), length });
      return new util.TextDecoder('utf-8', { fatal: false }).decodeWithStream(new Uint8Array(buffer, 0, count));
    } catch (error) { return 'Log unavailable: ' + String(error); }
    finally { if (file) fs.closeSync(file); }
  }
  private refresh(): void {
    this.contents = 'APP — last 64 KiB\n' + this.tail('ollama-app.log') +
      '\n\nCore — last 64 KiB\n' + this.tail('.ollama/logs/server.log') +
      '\n\nIndependent llama-server — last 64 KiB\n' + this.tail('desktop/llama-server.log');
  }
  aboutToAppear(): void { this.refresh(); }
  build() {
    Column({ space: 12 }) {
      Row({ space: 12 }) {
        Text('View logs').fontSize(20).layoutWeight(1)
        Button('Refresh logs').onClick(() => this.refresh())
        Button('Close logs').onClick(() => { this.visible = false; })
      }.width('100%')
      Scroll() {
        Text(this.contents).fontSize(12).fontFamily('monospace').copyOption(CopyOptions.LocalDevice).width('100%')
      }.layoutWeight(1).width('100%')
    }.padding(20).width('100%').height('100%').backgroundColor($r('sys.color.ohos_id_color_background'))
  }
}
