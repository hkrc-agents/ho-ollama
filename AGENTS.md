# AGENTS.md

## First HarmonyOS build in this downstream fork

Use the `harmonyos` branch. Read [HARMONYOS.md](HARMONYOS.md) for prerequisites
and the complete commands, and [harmonyos/AGENTS.md](harmonyos/AGENTS.md)
for port-specific rules.

1. Check out [ho-llama.cpp](https://github.com/hkrc-agents/ho-llama.cpp) on
   `harmonyos`. Use that checkout for the runtime builder's `--llama-port` and
   `--runner-objects` arguments. Both forks provide the pinned source objects.
2. Install Git, Python 3.10+, CMake, Ninja or Make, Go 1.26.0, Node 22+ with npm,
   JDK 17 and the compatible HarmonyOS Command Line Tools/SDK. Set
   `HARMONY_NATIVE_SDK` and `PICO_GO` to the installed SDK and Go compiler.
   [Official HarmonyOS Command Line Tools](https://developer.huawei.com/consumer/en/doc/harmonyos-guides/ide-commandline-get)
3. Run `harmonyos/tools/build_runtime.py`, then `build_official_app.py`,
   `build_hap.py` and `package_full_hap.py` as documented. Use fresh output
   directories under the checkout's Git-ignored `build/` or `target/`. The full Go APP/HAP path is x86_64.
   ARM64 uses the runtime builder's `--arch arm64 --native-only` path.
4. Run `python3 -m unittest discover -s harmonyos/tests -v`. For the
   maintained executable route and the archived HAP-managed SO design, see
   [service launch designs](harmonyos/SERVICE_LAUNCH_DESIGNS.md).

## Building

For a full build from the repository root:

```sh
cmake -B build .
cmake --build build --parallel 8
./ollama serve
```

For quick Go-only iteration against an existing native payload:

```sh
mkdir -p build
go build -o build/ollama .
go run . serve
```

See `docs/development.md` for prerequisites, platform notes, GPU backends, and
the full development workflow.
