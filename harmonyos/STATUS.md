# HarmonyOS port status

Imported from the maintained CPU adaptation bundles on 2026-11-04.
Upstream: https://github.com/ollama/ollama
Tested upstream base: `8a971df3bacec93944ec894f2939fd2567afc5dd`. The `harmonyos` branch retains this history;
the upstream default branch is not overwritten. Current upstream HEAD is not qualified by this import.

The existing implementation includes bounded emulator app/service tests and partial ARM64
build/device checks. Historical complete ARM64 numerical qualification did not pass;
independent mathematical diagnostics are not production qualification. Several storage,
lifecycle and retail/release checks remain open. No historical device settings or serials
are required to build this repository. No device tests were resumed for this export.

The production route uses supervised ELF processes. Native-child/Go shared-object experiments
are retained as explicitly optional sources, not silently selected as the default.
The Go application is Linux/amd64 Go runtime with OHOS C bindings and custom build tags,
not an official native GOOS port. Do not infer ARM64 Go support from native CPU builds.

CPU-only scope: no ho-llama-models dependency, fused-model ABI, NPU SDK or weights.
External official HarmonyOS SDK/toolchains and user-controlled signing remain required.
See VALIDATION.md for checks actually rerun on the imported sources.
