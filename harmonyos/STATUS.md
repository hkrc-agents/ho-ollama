# Supported build paths

The maintained HarmonyOS source targets CPU execution. Native CLI/server/static libraries can be cross-compiled for x86_64 or arm64; each target needs its matching SDK and libraries.
The application Go toolchain uses Linux/amd64 runtime with OHOS C bindings and the harmonyos build tag; it is not a native GOOS port. The documented Go APP/HAP integration is x86_64.
The ArkTS/NAPI SDK example supports explicit x86_64 and arm64 source/library inputs.

Host regressions, compilation, device startup, model numerical correctness and retail signing are separate checks. Do not infer ARM64 model numerical or retail/lifecycle qualification from a successful build.
Use the build and test commands in ../HARMONYOS.md. Keep generated results in an external output directory.
