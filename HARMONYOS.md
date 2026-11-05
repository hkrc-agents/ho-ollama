# ho-ollama: HarmonyOS PC CPU port

This official-upstream fork contains the HarmonyOS Go/React adapters,
ArkWeb/ArkTS HAP shell, model-storage and service supervision code, patches, tests and build tools.
The prepared Go/React changes are also applied in the normal upstream source locations.
Keep generic fixes separate from downstream product work when preparing future upstream submissions.
[Supported build paths](harmonyos/STATUS.md)

## Dependencies

Linux/WSL, Git, Python 3.10+, CMake, Ninja or Make, Go 1.26.0, Node 22+ (tested with 24), npm,
JDK 17, official HarmonyOS command-line tools/SDK (API23 for the existing HAP), and the
[ho-llama.cpp](https://github.com/hkrc-agents/ho-llama.cpp) `harmonyos` branch.
Dependencies are external; keep SDKs and generated results outside tracked source.
Obtain the SDK using the [official command-line tools instructions](https://developer.huawei.com/consumer/en/doc/harmonyos-guides/ide-commandline-get). Install [Go](https://go.dev/dl/), [Node.js](https://nodejs.org/en/download) and JDK 17 externally; select their paths explicitly.

## Matched runtime and APP

Choose new output directories. Both repositories may be cloned anywhere; pass their paths explicitly.
The runtime recipe creates clean source trees from the tested Git base objects, then applies the
owned platform patches. It never copies a dirty upstream cache. The fork's `harmonyos/` adapters
are the maintained preparation inputs; generated prepared trees are build artifacts.

```bash
export HARMONY_NATIVE_SDK=/path/to/sdk/default/openharmony/native
export PICO_GO=/path/to/go1.26/bin/go
export PATH=/path/to/node24/bin:$PATH
python3 harmonyos/tools/build_runtime.py \
  --llama-port /path/to/ho-llama.cpp --runner-objects /path/to/ho-llama.cpp \
  --sdk "$HARMONY_NATIVE_SDK" --go "$PICO_GO" \
  --cache /path/to/build-cache --out /path/to/runtime
# Read the prepared Ollama path from runtime/source-selection.json.
python3 harmonyos/tools/build_official_app.py \
  --source /path/from/source-selection/ollama \
  --runtime-payload /path/to/runtime/payload --out /path/to/app-runtime
```

Use `--tls --router` on the runtime builder for the existing LibreSSL/explicit CA-directory
and subprocess route. LibreSSL is fetched by the pinned upstream CMake recipe and its
archive SHA256 is checked. The native CA directory is supplied explicitly by the platform host.
Without `--tls`, HTTPS is unavailable; do not describe that minimal build as a complete product.
ARM64 native builds require `--arch arm64 --native-only`; the Go/HAP recipe remains x86_64.
The runtime build emits native libraries/headers, binaries and hashes. The APP build performs
a real npm/React build before Go embeds the frontend; it does not synthesize empty assets.
All outputs are build candidates, not inherited device qualification.

## HAP and full packaging

```bash
python3 harmonyos/tools/build_hap.py --out /path/to/hap-build \
  --tools-dir /path/to/command-line-tools/bin --java-home /path/to/jdk17 \
  --sdk-root /path/to/sdk --mode debug
python3 harmonyos/tools/package_full_hap.py \
  --native harmonyos/hap/entry/build/default/outputs/default/entry-default-unsigned.hap \
  --build-record /path/to/hap-build/identity.json --runtime /path/to/app-runtime \
  --sdk /path/to/sdk/default/openharmony --java /path/to/jdk17/bin/java \
  --out /path/to/packaged
```

The new full packager creates the HNP from the complete hash-checked payload and preserves
SDK HAP contents; it needs no historical signed package or frozen private runtime.
Outputs are unsigned. Configure personal signing externally; no certificates are in the repository.
The public tree contains source and static test fixtures only. Keep output packages, logs and build receipts in the selected output directories.

## Validation

```bash
python3 -m unittest discover -s harmonyos/tests -v
# In app/ui/app, after npm ci and route generation:
npm run test -- --run
npm run build
python3 harmonyos/tools/test_model_storage.py \
  --web /path/to/prepared-ollama/app/ui/app --out /path/to/storage-tests
# In the prepared source, after frontend dist has been built:
python3 harmonyos/tools/run_app_host_tests.py \
  --source /path/to/prepared-ollama --go "$PICO_GO" --out /path/to/app-host-check
```

The independent HNP test app is built from [harmonyos/test-client](harmonyos/test-client/README.md); it needs no private scaffold, model or runtime binary in the source tree. Run its host tests with `node --test harmonyos/test-client/tests/*.test.mjs`.

For deliberate device tests, `harmonyos/tools/harmonyos_ui.py` and `run_hnp_automation.py`
accept `HDC` from the environment. They require an explicitly selected device and the
corresponding installed test client. They are not part of the offline build or a claim of
complete device acceptance. No model, SDK, device credentials or artifact path is bundled.
