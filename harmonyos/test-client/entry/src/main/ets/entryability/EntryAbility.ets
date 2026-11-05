import { UIAbility, Want } from '@kit.AbilityKit';
import { window } from '@kit.ArkUI';
import client from 'libclienthnp.so';

// Debug automation host, not a shell. Commands execute under this application's UID.
export default class EntryAbility extends UIAbility {
  private dispatch(want: Want): void {
    try {
      const id = want.parameters?.['runId'];
      if (typeof id !== 'string') return;
      if (want.parameters?.['action'] === 'cancel') {
        AppStorage.setOrCreate('clientReport', client.cancel(id));
        return;
      }
      if (want.parameters?.['action'] === 'input') {
        const text = want.parameters?.['stdin'];
        AppStorage.setOrCreate('clientReport', client.sendInput(id,
          typeof text === 'string' ? text : '', want.parameters?.['eof'] === true));
        return;
      }
      const encoded = want.parameters?.['argv'];
      const input = want.parameters?.['stdin'];
      const timeout = want.parameters?.['timeoutMs'];
      if (typeof encoded !== 'string') throw new Error('argv must be a JSON array');
      const args = JSON.parse(encoded) as string[];
      if (!Array.isArray(args) || args.some((v: string) => typeof v !== 'string')) {
        throw new Error('argv must contain strings');
      }
      AppStorage.setOrCreate('clientReport', client.run(this.context.filesDir, id, args,
        typeof input === 'string' ? input : '', typeof timeout === 'number' ? timeout : 30000,
        want.parameters?.['interactive'] === true));
    } catch (error) { AppStorage.setOrCreate('clientReport', 'Rejected: ' + String(error)); }
  }
  onCreate(want: Want): void { this.dispatch(want); }
  onNewWant(want: Want): void { this.dispatch(want); }
  onWindowStageCreate(stage: window.WindowStage): void { stage.loadContent('pages/Index'); }
}
