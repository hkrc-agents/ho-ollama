# Ollama 鸿蒙 PC 应用

2026-10-20 CPU增量：Settings新增登录自启动系统状态/说明入口，模拟器状态查询、原生
页面刷新/返回及CPU回归通过；不提供虚假本地开关，真实登录触发仍待验收。
配套公共HNP自动化宿主见[Client HAP](../../../../apps/hnp-client/README.md)，完整步骤见
[CPU执行计划](../../../../design/HARMONYOS_CPU_EXECUTION_PLAN.md)，OMC/NNRT接入暂缓。

2026-10-22：本文源码已归入`repos/ollama/harmonyos/hap`，旧`apps/ollama`为兼容链接。
下列命令均以私有协调仓根目录/该兼容路径为基准，不表示本HAP已能脱离协调仓独立构建。
最新自启动、通知及生命周期结果以[集中验收](../../../../exp/HARMONYOS_CPU_FINAL_ACCEPTANCE_20261022.md)
为准；旧段落中的待验状态仅代表当时检查点。

独立 bundle `com.pico.ollama`。正式入口为 `OfficialIndex.ets`：ArkWeb 承载官方
React UI，复用官方 Go APP 服务、SQLite 历史与核心 Ollama。ArkTS 只负责平台壳；
原 `Index.ets` 三页仅保留为历史功能原型，不是用户已接受的正式界面。
HarmonyOS 用 Models 页替代 Apps：显示模型大小/参数/量化/加载状态，调用原核心接口
加载、卸载、确认删除。已新增调用原核心 `/api/pull` 的下载/进度/取消重试，以及
原 `/api/show` 模型详情；设备验收范围见迁移清单的 model acquisition 轮次。
Chat `+` 已接通系统文件选择器，原 ChatForm/文件校验不重写。
设置中的 macOS 集成区域已按平台能力隐藏；外部模型目录、账号与正式更新仍待适配，
对应未实现控件明确禁用。上下文 1k 是当前模拟器测试默认，不是所有设备的产品配置。
原生桥接负责应用身份下的 HNP 进程启动、日志、私有认证和 HDC 调试入口。
原 `experiments/harmonyos_network_hap` 保留用于历史测试，不作为新产品源码。

当前目标是 x86_64 模拟器的初步验收。ARM64 真机、外部模型目录授权、GGUF 导入 UI、
存储异常完整矩阵和正式签名仍需后续交付，不能据此声称完整发布。多会话历史复用官方 SQLite。

## 构建

安装 Linux JDK17 和 Go1.26，SDK 位于 `/mnt/d/lab/command-line-tools`。
默认持久工具目录是 `~/.cache/pico/toolchains/{jdk17,go1.26}`，源码和 native build
也保存在 `~/.cache/pico`；不依赖历史 `/tmp` 文件。

```bash
python3 scripts/build_harmonyos_ollama_runtime.py \
  --out artifacts/harmonyos_product_20261014/runtime
cd apps/ollama
/mnt/d/lab/command-line-tools/bin/ohpm install
PATH="$HOME/.cache/pico/toolchains/jdk17/bin:$PATH" \
  /mnt/d/lab/command-line-tools/bin/hvigorw --no-daemon assembleHap
bash pack.sh ../../artifacts/harmonyos_product_20261014/runtime/payload \
  ../../artifacts/harmonyos_product_20261014/package
```

上面是早期原型 runtime 的构建方式。正式 UI 包必须先加入官方 APP 服务，不能
只打包旧 payload：

```bash
python3 scripts/build_harmonyos_official_app.py \
  --runtime-payload artifacts/harmonyos_product_20261014/runtime/payload \
  --out artifacts/harmonyos_official_app_20261014/runtime
python3 scripts/build_harmonyos_ollama_hap.py \
  --out artifacts/harmonyos_official_app_20261014/hap
bash apps/ollama/pack.sh artifacts/harmonyos_official_app_20261014/runtime/payload \
  artifacts/harmonyos_official_app_20261014/package
```

在仓库根目录执行；需先完成 `apps/ollama` 的 HAP 构建。原版 React 通过 npm lockfile
构建并嵌入 Go `ollama-app`；不需要在设备上运行 Node 或 Go 编译器。

后续 HAP 构建使用上面的串行 wrapper，保留输入 hash 并拒绝编译期间修改源文件。
升级部署通过 `scripts/deploy_harmonyos_ollama_app.py` 先请求旧 HAP 停止服务再安装；
旧 Go HNP 进程可能跨 HAP 替换存活，验收仍需检查应用内记录的实际运行 hash。

附件桥接位于 `resources/rawfile/harmony-bridge.js`，通过 DocumentViewPicker 返回
用户选择的 URI、浏览器 FileReader 返回标准数据；每次最多 10 个文件、单文件
16 MiB、合计 32 MiB。边界先在适配层检查，官方文件类型/模型能力校验继续生效。
`export-attachment-fixture` Want 仅为当前 debug HDC 验收导出固定测试文本，不是产品
文件管理接口；正式发布前与其他调试 Wants 一起收窄。

`runtime-identity.json` 保存上游 commit 和产物 hash。包内带 Ollama/llama.cpp 及 vendor
许可证。Ollama runner 保持固定匹配版本；鸿蒙平台补丁独立于上游兼容 hooks。
`sdk/include` 与 `sdk/lib` 输出供应用进程内集成 llama.cpp，不要求启动 Ollama 服务。

## HDC 调用

```bash
HDC=/mnt/d/lab/command-line-tools/sdk/default/openharmony/toolchains/hdc
"$HDC" -t 127.0.0.1:5555 install -r artifacts/harmonyos_product_20261014/package/ollama.hap
"$HDC" -t 127.0.0.1:5555 shell \
  'aa start -b com.pico.ollama -a EntryAbility --ps action start'
"$HDC" -t 127.0.0.1:5555 shell \
  'aa start -b com.pico.ollama -a EntryAbility --ps action command --ps argv '\''["list"]'\'''
"$HDC" -t 127.0.0.1:5555 shell -b com.pico.ollama \
  'cat data/storage/el2/base/haps/entry/files/ollama-command.exit; cat data/storage/el2/base/haps/entry/files/ollama-command.log'
"$HDC" -t 127.0.0.1:5555 shell \
  'aa start -b com.pico.ollama -a EntryAbility --ps action stop'
```

命令异步执行，读取输出前等待 exit 文件出现；新命令前删除上次的 exit 标记。
这条链路为 HDC→HAP→原版 HNP Ollama，没有重新实现 CLI。
公共 HNP 直接执行的身份与 stdio 行为需要单独记录，不视为应用身份执行。
调试 Want 用于当前 debug 模拟器验证，正式发布前需收窄导出接口和调试权限。

自动验证入口：

```bash
python3 scripts/verify_harmonyos_ollama_app.py \
  --evidence artifacts/harmonyos_product_20261014/acceptance \
  --model models/qwen3.5-0.8b-q4_k_m.gguf
```

测试串行，保留原始 CLI 输出和退出码；完成后停止服务并移除自己的端口转发。

官方 APP 专项（先通过上面的 CLI/模型导入验证）：

```bash
python3 scripts/verify_harmonyos_official_app.py \
  --runtime-identity artifacts/harmonyos_official_app_20261014/runtime-r3/official-app-identity.json \
  --evidence artifacts/harmonyos_official_app_20261014/app-acceptance-r3
python3 scripts/verify_harmonyos_official_ui.py \
  --app-results artifacts/harmonyos_official_app_20261014/app-acceptance-r3/results.json \
  --evidence artifacts/harmonyos_official_app_20261014/ui-acceptance-r3-rerun
```

identity 路径必须对应本次安装包，而非任意历史版本。APP 专项检查鉴权、真实流式
聊天、SQLite 重启持久化和实际 HNP 文件 hash；UI 专项通过 HDC 点选官方页面。
`verify_harmonyos_ollama_ui.py` 只适用于旧 ArkTS 原型，不适用于正式入口。
