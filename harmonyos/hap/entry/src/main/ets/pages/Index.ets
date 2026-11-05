import { AppRuntime } from '../services/AppRuntime';
import { ChatMessage, ModelInfo, OllamaApi } from '../services/OllamaApi';
import bridge from 'libollamabridge.so';

@Entry
@Component
struct Index {
  @StorageLink('serviceStatus') status: string = 'stopped';
  @State models: ModelInfo[] = [];
  @State loaded: string[] = [];
  @State selected: string = '';
  @State prompt: string = '';
  @State transcript: string = '';
  @State thinking: boolean = false;
  @State generating: boolean = false;
  @State busy: boolean = false;
  @State error: string = '';
  @State port: string = '11434';
  @State logs: string = '';
  private messages: ChatMessage[] = [];
  private cancelled: boolean = false;
  private api: OllamaApi = new OllamaApi(11434);
  aboutToAppear(): void {
    this.port = String(AppRuntime.port); this.api = AppRuntime.api();
    AppRuntime.store.get('chatMessages', '[]').then((value) => {
      this.messages = JSON.parse(String(value)) as ChatMessage[];
      this.transcript = this.messages.map((message: ChatMessage) => `${message.role}：${message.content}\n`).join('\n');
    }).catch((error: Error) => { this.error = String(error); });
  }
  aboutToDisappear(): void { this.api.cancel(); }
  private async refresh(): Promise<void> {
    this.error = '';
    try {
      this.api = AppRuntime.api();
      this.models = await this.api.models();
      const running = await this.api.loaded();
      this.loaded = running.map((item: ModelInfo) => item.name);
      if (this.selected.length === 0 && this.models.length > 0) this.selected = this.models[0].name;
    } catch (error) { this.error = String(error); }
  }
  private async service(start: boolean): Promise<void> {
    this.busy = true; this.error = '';
    try {
      if (start) { await AppRuntime.start(); await this.refresh(); }
      else { this.api.cancel(); await AppRuntime.stop(); this.loaded = []; }
    } catch (error) { this.error = String(error); }
    finally { this.busy = false; }
  }
  private async send(): Promise<void> {
    if (this.prompt.trim().length === 0 || this.selected.length === 0 || this.generating) return;
    this.generating = true; this.error = ''; this.cancelled = false;
    const text = this.prompt;
    this.prompt = '';
    this.messages.push({ role: 'user', content: text });
    this.transcript += `\n你：${text}\n\n${this.selected}：`;
    let response: string = '';
    try {
      await this.api.chat(this.selected, this.messages, this.thinking, (chunk: string) => {
        response += chunk; this.transcript += chunk;
      });
    } catch (error) { if (!this.cancelled) this.error = String(error); }
    finally {
      if (response.length > 0) this.messages.push({ role: 'assistant', content: response });
      this.transcript += '\n'; this.generating = false;
      if (this.cancelled) this.transcript += '[已停止生成]\n';
      try {
        await AppRuntime.store.put('chatMessages', JSON.stringify(this.messages));
        await AppRuntime.store.flush();
      } catch (error) { this.error = `保存会话失败: ${String(error)}`; }
    }
  }
  private async modelAction(model: ModelInfo, action: string): Promise<void> {
    this.busy = true; this.error = '';
    try {
      if (action === 'delete') await this.api.remove(model.name);
      else await this.api.load(model.name, action === 'unload');
      await this.refresh();
    } catch (error) { this.error = String(error); }
    finally { this.busy = false; }
  }
  build() {
    Column({ space: 12 }) {
      Row() {
        Text('Ollama').fontSize(28).fontWeight(FontWeight.Bold)
        Blank()
        Text(this.status).fontSize(14)
        Button('启动').enabled(!this.busy && !this.generating).margin({ left: 16 }).onClick(() => { this.service(true); })
        Button('停止').enabled(!this.busy && !this.generating).margin({ left: 8 }).onClick(() => { this.service(false); })
      }.width('100%')
      if (this.error.length > 0) { Text(this.error).fontColor('#b42318').width('100%') }
      Tabs() {
        TabContent() {
          Column({ space: 12 }) {
            Row({ space: 12 }) {
              Select(this.models.map((item: ModelInfo): SelectOption => { return { value: item.name }; }))
                .value(this.selected).enabled(!this.generating)
                .onSelect((index: number) => { this.selected = this.models[index].name; })
              Button('刷新模型').onClick(() => { this.refresh(); })
              Toggle({ type: ToggleType.Switch, isOn: this.thinking }).onChange((on: boolean) => { this.thinking = on; })
              Text('思考')
              Button('新建会话').enabled(!this.generating).onClick(async () => {
                this.messages = []; this.transcript = '';
                await AppRuntime.store.put('chatMessages', '[]'); await AppRuntime.store.flush();
              })
            }
            Scroll() { Text(this.transcript.length > 0 ? this.transcript : '启动服务并选择模型后开始对话。')
              .width('100%').copyOption(CopyOptions.InApp).fontSize(16) }.layoutWeight(1).width('100%')
            TextArea({ placeholder: '输入消息', text: this.prompt }).height(100)
              .onChange((value: string) => { this.prompt = value; })
            Row({ space: 12 }) {
              Button('发送').enabled(!this.generating && this.status === 'ready').onClick(() => { this.send(); })
              Button('停止生成').enabled(this.generating).onClick(() => { this.cancelled = true; this.api.cancel(); })
            }
          }.padding(16).height('100%')
        }.tabBar('Chat')
        TabContent() {
          Column({ space: 12 }) {
            Button('刷新').enabled(!this.busy).onClick(() => { this.refresh(); })
            Text('卸载释放运行内存；删除移除本地模型。共享权重可能使实际释放空间小于模型大小。').fontSize(13)
            List({ space: 12 }) {
              ForEach(this.models, (model: ModelInfo) => {
                ListItem() {
                  Column({ space: 8 }) {
                    Text(model.name).fontWeight(FontWeight.Bold)
                    Text(`${(model.size / 1048576).toFixed(1)} MiB · ${model.details?.parameter_size ?? ''} · ${model.details?.quantization_level ?? ''}`)
                    Text(this.loaded.includes(model.name) ? '已加载' : '已下载，未加载')
                    Row({ space: 12 }) {
                      Button('加载').enabled(!this.busy && !this.generating).onClick(() => { this.modelAction(model, 'load'); })
                      Button('卸载').enabled(!this.busy && !this.generating).onClick(() => { this.modelAction(model, 'unload'); })
                      Button('删除').enabled(!this.busy && !this.generating).onClick(() => {
                        AlertDialog.show({ title: '删除模型', message: `确认删除 ${model.name}？`,
                          primaryButton: { value: '取消', action: () => {} },
                          secondaryButton: { value: '删除', action: () => { this.modelAction(model, 'delete'); } } });
                      })
                    }
                  }.padding(16).alignItems(HorizontalAlign.Start).width('100%').backgroundColor('#f3f4f6').borderRadius(12)
                }
              }, (model: ModelInfo) => model.name)
            }.layoutWeight(1)
          }.padding(16).height('100%')
        }.tabBar('模型管理')
        TabContent() {
          Column({ space: 16 }) {
            Text('模型存放位置').fontWeight(FontWeight.Bold)
            Text(AppRuntime.modelsDir).copyOption(CopyOptions.InApp)
            Text('当前使用应用私有目录。外部目录授权与模型迁移将在后续版本开放。').fontSize(13)
            Text('本地 API 端口')
            TextInput({ text: this.port }).type(InputType.Number).width(240).onChange((value: string) => { this.port = value; })
            Button('保存设置').enabled(!this.busy).onClick(async () => {
              try { await AppRuntime.save(Number(this.port)); this.api = AppRuntime.api(); this.error = ''; }
              catch (error) { this.error = String(error); }
            })
            Text(`API：http://127.0.0.1:${AppRuntime.port} · CPU · 上下文 1024 · 线程 4`)
            Text('Ollama for HarmonyOS 0.1.0 · 模拟器初步验证版')
            Button('查看服务日志').onClick(() => { this.logs = bridge.logs(AppRuntime.context.filesDir); })
            Scroll() { Text(this.logs).fontSize(12).copyOption(CopyOptions.InApp).width('100%') }.layoutWeight(1)
          }.padding(16).height('100%').alignItems(HorizontalAlign.Start)
        }.tabBar('设置')
      }.layoutWeight(1).width('100%')
    }.padding(24).width('100%').height('100%').backgroundColor('#ffffff')
  }
}
