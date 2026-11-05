#!/usr/bin/env bash
set -euo pipefail
app_root="$(cd "$(dirname "$0")" && pwd)"
runtime_payload="$(realpath "${1:?runtime payload directory}")"
output_dir="$(realpath -m "${2:?new output directory}")"
sdk_oh="${HARMONY_SDK_OH:?Set HARMONY_SDK_OH}"
java_bin="${PICO_JAVA:-java}"
mkdir -p "$output_dir"
pack_stage="$(mktemp -d /tmp/pico-ollama-package-XXXXXX)"
cp -R "$runtime_payload" "$pack_stage/payload"
test -x "$pack_stage/payload/bin/ollama"
test -x "$pack_stage/payload/lib/ollama/llama-server"
node - "$pack_stage/payload/hnp.json" <<'JS'
const fs = require('fs'), path = require('path');
const bins = ['ollama', 'llama-cli', 'llama-bench', 'llama-quantize', 'llama-simple'];
const links = bins.filter(name => fs.existsSync(path.join(path.dirname(process.argv[2]), 'bin', name)))
  .map(name => ({source: 'bin/' + name, target: name}));
fs.writeFileSync(process.argv[2], JSON.stringify({type:'hnp-config',name:'ollama',version:'1.0',
  install:{links} }));
JS
mkdir -p "$pack_stage/hnp/x86_64"
"$sdk_oh/toolchains/hnpcli" pack -i "$pack_stage/payload" -o "$pack_stage/hnp/x86_64"
mkdir -p "$pack_stage/hap"
(
  cd "$pack_stage/hap"
  "$(dirname "$java_bin")/jar" xf "$app_root/entry/build/default/outputs/default/entry-default-unsigned.hap"
)
"$java_bin" -jar "$sdk_oh/toolchains/lib/app_packing_tool.jar" --mode hap \
  --json-path "$pack_stage/hap/module.json" --ets-path "$pack_stage/hap/ets" \
  --lib-path "$pack_stage/hap/libs" --resources-path "$pack_stage/hap/resources" \
  --index-path "$pack_stage/hap/resources.index" --pack-info-path "$pack_stage/hap/pack.info" \
  --hnp-path "$pack_stage/hnp" --out-path "$output_dir/ollama.hap" --force true
sha256sum "$output_dir/ollama.hap" "$pack_stage/payload/bin/ollama" \
  "$pack_stage/payload/lib/ollama/llama-server" > "$output_dir/SHA256SUMS"
echo "Retained package sources: $pack_stage"
