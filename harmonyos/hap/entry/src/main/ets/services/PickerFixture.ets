// Debug HDC acceptance helper: export only a fixed, non-sensitive test document.
// Uses the system Save dialog, never an arbitrary filesystem path from a Want.
import { picker, fileIo as fs } from '@kit.CoreFileKit';
import { common } from '@kit.AbilityKit';

export async function exportAttachmentFixture(context: common.UIAbilityContext): Promise<void> {
  const options = new picker.DocumentSaveOptions();
  options.newFileNames = ['pico-attachment-check-20261014.txt'];
  const paths = await new picker.DocumentViewPicker(context).save(options);
  if (paths.length !== 1) return;
  const file = fs.openSync(paths[0], fs.OpenMode.READ_WRITE | fs.OpenMode.CREATE | fs.OpenMode.TRUNC);
  try { fs.writeSync(file.fd, 'The verification code in this attachment is 48271.\n'); }
  finally { fs.closeSync(file); }
  const record = fs.openSync(context.filesDir + '/attachment-fixture.json', fs.OpenMode.READ_WRITE | fs.OpenMode.CREATE | fs.OpenMode.TRUNC);
  try { fs.writeSync(record.fd, JSON.stringify({ uri: paths[0], filename: options.newFileNames[0] })); }
  finally { fs.closeSync(record); }
}
