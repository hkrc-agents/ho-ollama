#!/usr/bin/env bash
set -euo pipefail
# Requires the pinned upstream tree plus ollama_harmony_manifest_copy.patch.
source_dir="${1:?Ollama source directory}"
output_file="${2:?absolute output binary path}"
sdk_native="${HARMONY_NATIVE_SDK:?Set HARMONY_NATIVE_SDK to the native SDK directory}"
go_bin="${PICO_GO:-go}"
test -f "$source_dir/manifest/copy_policy_harmony.go"
cd "$source_dir"
export GOOS=linux GOARCH=amd64 CGO_ENABLED=1 GOTOOLCHAIN=local
export CC="$sdk_native/llvm/bin/clang --target=x86_64-linux-ohos --sysroot=$sdk_native/sysroot"
export CXX="$sdk_native/llvm/bin/clang++ --target=x86_64-linux-ohos --sysroot=$sdk_native/sysroot"
# Experimental Linux Go runtime + OHOS C bindings, not a native GOOS port.
# Prefer the target libc resolver without requiring a diagnostic GODEBUG setting.
# Keep upstream net unchanged; CGO_ENABLED=1 is required above.
exec "$go_bin" build -tags=harmonyos,netcgo -o "$output_file" .
