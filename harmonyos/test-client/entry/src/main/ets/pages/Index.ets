import { common } from '@kit.AbilityKit';
import { picker, fileIo as fs } from '@kit.CoreFileKit';
import hash from '@ohos.file.hash';
import statvfs from '@ohos.file.statvfs';
import util from '@ohos.util';
import client from 'libclienthnp.so';
import { parseCommand } from '../services/CommandLine';
import { insertNewLine } from '../services/InputEditing';
import { completeUtf8Length, displayOutput } from '../services/OutputDisplay';
interface CommandResult {
  complete: boolean;
  run_id: string;
  stdout_base64: string;
  stderr_base64: string;
  exit_code: number;
  signal: number;
  spawn_error: number;
  timed_out: boolean;
  cancelled: boolean;
  output_or_io_limited: boolean;
  elapsed_ms: number;
}
interface LiveResult {
  stdout_base64?: string;
  stderr_base64?: string;
  stdin_closed?: boolean;
}
@Entry
@Component
struct Index {
  @StorageLink('clientReport') report: string = 'Ready for HDC test requests';
  @State copying: boolean = false;
  @State command: string = 'ollama list';
  @State input: string = '';
  @State running: boolean = false;
  @State stdout: string = '';
  @State stderr: string = '';
  @State stdinClosed: boolean = true;
  private restart: boolean = false;
  private runId: string = '';
  private timer: number = -1;
  private reading: boolean = false;
  private root: string = '';
  private deadline: number = 0;
  private lastError: string = '';
  private inputController: TextAreaController = new TextAreaController();
  private selectionStart: number = 0;
  private selectionEnd: number = 0;
  private commandEnterHeld: boolean = false;
  private stdinEnterHeld: boolean = false;
  aboutToDisappear(): void {
    this.restart = false;
    if (this.running) client.cancel(this.runId);
    if (this.timer !== -1) clearInterval(this.timer);
    this.timer = -1;
  }
  private decode(value: string, complete: boolean = false): string {
    if (value.length === 0) return '';
    const bytes = new util.Base64Helper().decodeSync(value);
    const size = complete ? bytes.length : completeUtf8Length(bytes);
    return displayOutput(new util.TextDecoder('utf-8', { fatal: false }).decodeToString(bytes.slice(0, size)));
  }
  private async poll(): Promise<void> {
    if (!this.running || this.reading) return;
    if (Date.now() > this.deadline) {
      client.cancel(this.runId);
      this.report = 'Result unavailable; cancellation requested, cleanup unconfirmed. ' + this.lastError;
      this.running = false; clearInterval(this.timer); this.timer = -1; return;
    }
    this.reading = true;
    try {
      const live = JSON.parse(client.snapshot(this.runId)) as LiveResult;
      if (live.stdout_base64 !== undefined) this.stdout = this.decode(live.stdout_base64);
      if (live.stderr_base64 !== undefined) this.stderr = this.decode(live.stderr_base64);
      if (live.stdin_closed !== undefined) this.stdinClosed = live.stdin_closed;
      // Capture filesDir in the UI event, not a timer's ambient UI context.
      const path = this.root + '/' + this.runId + '.result.json';
      // Most jobs do not have a result yet. Avoid issuing an asynchronous open
      // against an absent file on every tick; keep the worker monitor live.
      if (!fs.accessSync(path)) return;
      const raw = await fs.readText(path);
      const result = JSON.parse(raw) as CommandResult;
      if (!result.complete || result.run_id !== this.runId) return;
      this.stdout = this.decode(result.stdout_base64, true);
      this.stderr = this.decode(result.stderr_base64, true);
      this.report = 'Exit ' + result.exit_code.toString() + ' · signal ' + result.signal.toString() +
        ' · ' + result.elapsed_ms.toString() + ' ms' +
        (result.spawn_error ? ' · spawn error ' + result.spawn_error.toString() : '') +
        (result.cancelled ? ' · cancelled' : '') + (result.timed_out ? ' · timed out' : '') +
        (result.output_or_io_limited ? ' · output/I/O limited' : '');
      this.running = false;
      this.stdinClosed = true;
      clearInterval(this.timer); this.timer = -1;
      if (this.restart) {
        this.restart = false;
        // Let finally release the read guard before the next job starts.
        setTimeout(() => this.run(), 0);
      }
    } catch (error) {
      // A missing or partially written result is not completion. The existing
      // native runner owns timeout/cancellation/reaping and publishes the result.
      this.lastError = String(error);
    } finally { this.reading = false; }
  }
  private run(): void {
    if (this.copying) return;
    if (this.running) {
      this.restart = true;
      this.report = 'Stopping previous command before starting the new command · ' + client.cancel(this.runId);
      return;
    }
    try {
      const args = parseCommand(this.command);
      const context = getContext(this) as common.UIAbilityContext;
      this.root = context.filesDir;
      const id = 'manual_' + util.generateRandomUUID().replace(/-/g, '');
      const response = client.run(context.filesDir, id, args, this.input, 120000, true);
      this.report = response;
      if (response !== 'Accepted') return;
      this.runId = id; this.stdout = ''; this.stderr = ''; this.running = true;
      this.input = ''; this.stdinClosed = false;
      this.deadline = Date.now() + 125000; this.lastError = '';
      this.report = 'Running · stdin open · live output · timeout 120 s';
      this.timer = setInterval(() => { this.poll(); }, 500);
    } catch (error) { this.report = 'Rejected: ' + String(error); }
  }
  private cancel(): void {
    this.restart = false;
    if (this.running) this.report = client.cancel(this.runId);
  }
  private send(eof: boolean): void {
    if (!this.running || this.stdinClosed) return;
    const response = client.sendInput(this.runId, eof ? '' : this.input + '\n', eof);
    this.report = response;
    if (response === 'Input queued') this.input = '';
    if (response === 'EOF queued after pending input') this.stdinClosed = true;
  }
  private enter(event: KeyEvent, command: boolean): boolean {
    // Handle after the IME, not in onKeyPreIme: candidate confirmation must
    // remain the input method's responsibility. Consume native text insertion.
    if (event.keyCode !== 2054 && event.keyCode !== 2119) {
      // Dispatch is attached to a wrapper, not the text component: explicitly
      // preserve native selection, deletion, navigation and shortcut handling.
      return this.getUIContext().dispatchKeyEvent(command ? 'client-command' : 'client-stdin', event);
    }
    if (event.type === KeyType.Up) {
      if (command) this.commandEnterHeld = false; else this.stdinEnterHeld = false;
    } else if (event.type === KeyType.Down) {
      const held = command ? this.commandEnterHeld : this.stdinEnterHeld;
      if (command) this.commandEnterHeld = true; else this.stdinEnterHeld = true;
      if (!held) { if (command) this.run(); else this.send(false); }
    }
    return true;
  }
  private newLine(): void {
    const edit = insertNewLine(this.input, this.selectionStart, this.selectionEnd);
    this.input = edit.text;
    this.selectionStart = edit.caret; this.selectionEnd = edit.caret;
    setTimeout(() => {
      focusControl.requestFocus('client-stdin');
      this.inputController.caretPosition(edit.caret);
    }, 0);
  }
  private async choose(): Promise<void> {
    if (this.copying || this.running) return;
    this.copying = true;
    const context = getContext(this) as common.UIAbilityContext;
    const temporary = context.filesDir + '/cli-model.part-' + util.generateRandomUUID();
    let input: fs.File | undefined = undefined;
    let output: fs.File | undefined = undefined;
    try {
      const options = new picker.DocumentSelectOptions(); options.maxSelectNumber = 1;
      const selected = await new picker.DocumentViewPicker(context).select(options);
      if (selected.length === 0) { this.report = 'Selection cancelled'; return; }
      input = fs.openSync(selected[0], fs.OpenMode.READ_ONLY | fs.OpenMode.NOFOLLOW);
      const size = fs.statSync(input.fd).size;
      if (size < 24 || size > 1024 * 1024 * 1024 || await statvfs.getFreeSize(context.filesDir) < size + 256 * 1024 * 1024) throw new Error('CLI fixture requires one GGUF up to 1 GiB and sufficient private space');
      this.report = 'Copying selected CLI model asynchronously…';
      output = fs.openSync(temporary, fs.OpenMode.CREATE | fs.OpenMode.READ_WRITE | fs.OpenMode.NOFOLLOW);
      await fs.copyFile(input.fd, output.fd); fs.fsyncSync(output.fd);
      if (fs.statSync(output.fd).size !== size) throw new Error('Model fixture size mismatch');
      fs.closeSync(output); output = undefined;
      const digest = await hash.hash(temporary, 'sha256');
      fs.renameSync(temporary, context.filesDir + '/cli-model.gguf');
      const note = fs.openSync(context.filesDir + '/cli-model-identity.json', fs.OpenMode.CREATE | fs.OpenMode.WRITE_ONLY | fs.OpenMode.TRUNC);
      try { fs.writeSync(note.fd, JSON.stringify({ size, sha256: digest, source: selected[0] })); fs.fsyncSync(note.fd); }
      finally { fs.closeSync(note); }
      this.report = 'CLI fixture ready: ' + size.toString() + ' bytes, SHA256 ' + digest;
    } catch (error) { this.report = String(error); }
    finally {
      if (input !== undefined) fs.closeSync(input);
      if (output !== undefined) fs.closeSync(output);
      try { fs.unlinkSync(temporary); } catch (_) {}
      this.copying = false;
    }
  }
  build() {
    Column({ space: 10 }) {
      Row({ space: 12 }) {
        Text('Public HNP test client').fontSize(24).layoutWeight(1)
        Button('Choose local CLI GGUF').enabled(!this.copying && !this.running).onClick(() => this.choose())
      }.width('100%')
      Text('Public HNP / system PATH / absolute executable · no implicit shell or PTY').fontSize(14)
      Row() {
        TextInput({ text: this.command, placeholder: 'Command, e.g. ollama list' })
        .id('client-command').onChange((value: string) => { this.command = value; })
        .enterKeyType(EnterKeyType.Go)
        .onSubmit(() => { if (!this.commandEnterHeld) this.run(); })
        .onBlur(() => { this.commandEnterHeld = false; })
      }.width('100%').onKeyEventDispatch((event: KeyEvent): boolean => this.enter(event, true))
      Row({ space: 12 }) {
        Button('Run').id('client-run').enabled(!this.copying).onClick(() => this.run())
        Button('Cancel').id('client-cancel').enabled(this.running).onClick(() => this.cancel())
      }.width('100%')
      Row() {
        TextArea({ text: this.input, controller: this.inputController,
        placeholder: 'STDIN · Enter sends · NL/CR inserts a line break' })
        .id('client-stdin').height(72).onChange((value: string) => { this.input = value; })
        .enterKeyType(EnterKeyType.Send)
        .onTextSelectionChange((start: number, end: number) => {
          this.selectionStart = start; this.selectionEnd = end;
        })
        .onSubmit((_key: EnterKeyType, event?: SubmitEvent) => {
          event?.keepEditableState();
          if (!this.stdinEnterHeld) this.send(false);
        })
        .onBlur(() => { this.stdinEnterHeld = false; })
      }.width('100%').onKeyEventDispatch((event: KeyEvent): boolean => this.enter(event, false))
      Row({ space: 12 }) {
        Button('NL/CR').id('client-newline').enabled(!this.copying).onClick(() => this.newLine())
        Button('EOF').id('client-eof').enabled(this.running && !this.stdinClosed).onClick(() => this.send(true))
      }.width('100%')
      Text(this.report).id('client-status').fontSize(14).copyOption(CopyOptions.LocalDevice)
      Text('stdout').fontSize(16).width('100%')
      Scroll() { Text(this.stdout || '(no stdout)').id('client-stdout').fontFamily('monospace').fontSize(14)
        .width('100%').copyOption(CopyOptions.LocalDevice) }.layoutWeight(1).width('100%').backgroundColor('#F5F5F5')
      Text('stderr').fontSize(16).width('100%')
      Scroll() { Text(this.stderr || '(no stderr)').id('client-stderr').fontFamily('monospace').fontSize(14)
        .width('100%').copyOption(CopyOptions.LocalDevice) }.layoutWeight(1).width('100%').backgroundColor('#F5F5F5')
      Text('120 s limit · 256 KiB per output stream · existing HDC automation remains available.').fontSize(12)
    }.padding(24).width('100%').height('100%')
  }
}
