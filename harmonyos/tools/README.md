# Downstream HarmonyOS APP build tools

These tools belong to the Ollama adapter bundle, not to the private optimization
repository. `prepare_official_app.py` and `build_official_app.py` resolve their
patches, Go/React adapters, source lock and C helpers relative to this bundle.
The private root's old script names are compatibility launchers only.

## Explicit inputs

```bash
HARMONY_NATIVE_SDK=/path/to/sdk/default/openharmony/native \
PICO_GO=/path/to/go/bin/go \
python3 tools/build_official_app.py --source /path/to/prepared-ollama \
  --runtime-payload /path/to/qualified-runtime/payload --out /path/to/new-output
```

Run from this `harmonyos/` bundle or use an absolute script path. Supply the
version-pinned, platform-prepared official checkout and a qualified matched CPU
payload with its adjacent `runtime-identity.json`. This APP builder is not a
replacement for constructing the underlying llama runner or preparing all CLI
platform patches. It checks both source commits and the actual payload hashes.

Frontend generation/build precedes Go APP compilation: `app/ui/app/dist` is a
real Go embed input. A clean source replica without those assets fails setup;
do not invent an empty placeholder to obtain a green test run. Use npm's lockfile
installation and the actual frontend build. `--skip-npm-ci` only reuses an already
installed matching dependency tree; it does not skip frontend compilation.
For a bounded APP-only candidate, `--preserve-cli` retains the hash-verified base
CLI instead of rebuilding it under a different source path. This choice is
recorded in the output identity; the default still rebuilds both executables.
When the base is a later APP payload, explicitly pass its `--base-app-identity`
instead of reconstructing a native runtime manifest. Its exact file hashes are
verified and its identity-file hash is retained alongside the original source pins.

Outputs include React assets, APP/CLI executables, native helpers, logs and
`official-app-identity.json`. They are **build-only artifacts** until individually
qualified; a different path can produce a different binary hash. This remains
Linux/amd64 Go runtime with OHOS C bindings, not an official native GOOS or ARM64
qualification. It contains no OMC engine, DDK or model weights.

## Matched runner compatibility

`runner_compat.cmake` adds Ollama's own compatibility/model/clef hooks to a pinned
llama.cpp runner. It is not a generic llama.cpp platform patch and must not be
included in the minimal upstream llama.cpp review export. `build_cli.sh` builds
the platform-tagged CLI using explicit `HARMONY_NATIVE_SDK` and `PICO_GO` inputs.
The manifest ordinary-file policy lives at `../manifest-copy.patch`.

Legacy experiment paths for these three files are compatibility links. The
private integration runner composes the two source owners; it does not make the
private NPU compiler or historical experiment templates a CPU build dependency.
Optional test-only tmpfile compatibility is separate, not an implicit libc change.

## HAP building and bounded repackaging

`build_hap.py --out <new-log-directory> --mode debug|release` builds the owned
`../hap` project. `--app`, `--tools-dir`, `--java-home` and `--sdk-root` support
explicit project/tool locations; no network-probe HAP template is copied.

`package_hap.py` composes a frozen SDK HAP and an explicitly qualified base HNP.
It preserves that base CLI/runner and permits only a separately verified Go APP
replacement; it is a **bounded incremental packager**, not a release mechanism
that silently accepts replacement of every runtime component. New full runtime
artifacts need separate composition and device qualification before promotion.
An explicit `--native-runtime-manifest` is the narrow exception for the pinned
OHOS TLS/router build: five native tools plus the LibreSSL notice, with actual
ELF ABI, source-closure hashes and payload hashes checked. It does not permit
arbitrary library replacements. The matching SDK HAP must contain the public
CertificateManager provider and dedicated `LLAMA_OHOS_CA_DIRECTORY` environment
bridge; Go's certificate settings remain unchanged. Debug/Release qualification
uses the same explicit contract and still checks every nested HNP payload hash.
`--sdk` and `--java` select packaging tools explicitly. Its extractor rejects
traversal, duplicate names, links, special files, privileged modes and existing
destination directories before extraction, preserving ordinary executable bits.
It does not import a private diagnostic/probe package.

## Publication boundary

A local portable bundle or successful build is not permission to publish it.
Retain upstream licenses and complete human review of new-code licensing,
trademarks, provenance, debug/release behavior and contribution policy. No tool
here creates a remote repository, commits, pushes or submits a PR.

## Portable test fixtures

Preparation also applies `../test-fixture-portability.patch`, strictly limited
to three upstream test files, and installs HarmonyOS-only updater/onboarding
boundary tests. This fixes mock installer names, capability-based onboarding expectations
and registry membership/count checks; it does not enable the desktop updater.
The mock transport tests restore the platform installer value after their
checker/download shutdown. Product `Installer` remains empty on HarmonyOS and
`IsUpdatePending()` remains false, even for a simulated downloaded update.
The onboarding boundary independently pins the private APP database location,
checks real marker completion, preserves CLI config bytes and refuses creation
of an empty APP database by the CLI; it does not infer expected behavior solely
from the implementation's platform predicate.

These are test-adapted downstream results, not unchanged-upstream passes. Retain
the original failure logs and run the default Linux config control separately.
The Linux host harness confines writes and mock HTTP requests; its passes do
not qualify Windows/macOS native tests or HarmonyOS signed update installation.
