import http from '@ohos.net.http';
import util from '@ohos.util';

export interface ModelDetails {
  parameter_size?: string;
  quantization_level?: string;
}
export interface ModelInfo {
  name: string;
  size: number;
  size_vram?: number;
  details?: ModelDetails;
}
interface ModelList { models: ModelInfo[]; }
export interface ChatMessage { role: string; content: string; }
interface ChatChunk { message?: ChatMessage; done?: boolean; error?: string; }
interface GenerationOptions { num_ctx: number; num_thread: number; num_gpu: number; num_predict?: number; }
interface GenerateRequest { model: string; stream: boolean; keep_alive: string | number; options: GenerationOptions; }
interface ChatRequest extends GenerateRequest { messages: ChatMessage[]; think: boolean; }

export class OllamaApi {
  private port: number;
  private active: http.HttpRequest | undefined = undefined;
  constructor(port: number) { this.port = port; }
  private async request(path: string, method: http.RequestMethod, body: string = ''): Promise<string> {
    const req = http.createHttp();
    try {
      const result = await req.request(`http://127.0.0.1:${this.port}${path}`, {
        method: method, header: { 'Content-Type': 'application/json' },
        extraData: body, expectDataType: http.HttpDataType.STRING,
        connectTimeout: 3000, readTimeout: 120000
      });
      if (result.responseCode < 200 || result.responseCode >= 300) {
        throw new Error(`HTTP ${result.responseCode}: ${String(result.result)}`);
      }
      return String(result.result);
    } finally { req.destroy(); }
  }
  async version(): Promise<string> {
    return this.request('/api/version', http.RequestMethod.GET);
  }
  async models(): Promise<ModelInfo[]> {
    const result = JSON.parse(await this.request('/api/tags', http.RequestMethod.GET)) as ModelList;
    return result.models;
  }
  async loaded(): Promise<ModelInfo[]> {
    const result = JSON.parse(await this.request('/api/ps', http.RequestMethod.GET)) as ModelList;
    return result.models;
  }
  async load(name: string, unload: boolean): Promise<void> {
    const body: GenerateRequest = {
      model: name, stream: false, keep_alive: unload ? 0 : '5m',
      options: { num_ctx: 1024, num_thread: 4, num_gpu: 0 }
    };
    await this.request('/api/generate', http.RequestMethod.POST, JSON.stringify(body));
  }
  async remove(name: string): Promise<void> {
    await this.request('/api/delete', http.RequestMethod.DELETE, JSON.stringify({ model: name }));
  }
  cancel(): void { this.active?.destroy(); this.active = undefined; }
  async chat(model: string, messages: ChatMessage[], think: boolean,
    onText: (text: string) => void): Promise<void> {
    if (this.active !== undefined) throw new Error('已有生成请求');
    const req = http.createHttp();
    this.active = req;
    const decoder = new util.TextDecoder('utf-8');
    let pending: string = '';
    let protocolError: string = '';
    let done: boolean = false;
    const consume = (line: string): void => {
      if (line.trim().length === 0) return;
      try {
        const chunk = JSON.parse(line) as ChatChunk;
        if (chunk.error) protocolError = chunk.error;
        if (chunk.message) onText(chunk.message.content);
        if (chunk.done) done = true;
      } catch (error) { protocolError = `流数据格式错误: ${String(error)}`; }
    };
    req.on('dataReceive', (data: ArrayBuffer) => {
      pending += decoder.decodeToString(new Uint8Array(data), { stream: true });
      let end = pending.indexOf('\n');
      while (end >= 0) {
        consume(pending.substring(0, end));
        pending = pending.substring(end + 1);
        end = pending.indexOf('\n');
      }
    });
    try {
      const body: ChatRequest = { model: model, messages: messages, think: think, stream: true,
        keep_alive: '5m', options: { num_ctx: 1024, num_predict: 256, num_thread: 4, num_gpu: 0 } };
      const code = await req.requestInStream(`http://127.0.0.1:${this.port}/api/chat`, {
        method: http.RequestMethod.POST, header: { 'Content-Type': 'application/json' },
        extraData: JSON.stringify(body),
        connectTimeout: 3000, readTimeout: 240000
      });
      pending += decoder.decodeToString(new Uint8Array(0), { stream: false });
      consume(pending);
      if (code !== 200) throw new Error(`HTTP ${code}: ${protocolError}`);
      if (protocolError.length > 0) throw new Error(protocolError);
      if (!done) throw new Error('生成连接提前结束');
    } finally {
      req.destroy();
      if (this.active === req) this.active = undefined;
    }
  }
}
