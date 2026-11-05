# ARM64 CPU declaration candidate

`arm64-cpu-mlx-headers.patch` is an explicit compile-only candidate. It is **not
applied by the default preparation/build path**, not installed on the emulator,
and not part of the minimal upstream review export.

The pinned Ollama tree imports MLX's dynamic declarations even for a CPU CLI.
The SDK's Clang 15.0.4 rejects the ARM64 `__bf16` declaration in the bundled MLX
header. This is a compiler/header problem, not evidence of failed inference or
unsupported hardware arithmetic. Raising the target ISA to hide it would not
establish compatibility with the actual PC.

The candidate affects only `__OHOS__`: like the existing non-ARM fallback, it
uses a two-byte opaque BF16 placeholder for unused dynamic function-pointer
declarations. It does not implement BF16 arithmetic, convert model weights,
change llama.cpp kernels, or enable an MLX backend. All other platforms retain
the original preprocessor branches. FP16 declarations remain unchanged.

The two changed headers come from pinned Ollama commit
`8a971df3bacec93944ec894f2939fd2567afc5dd`; retain their existing Apple/MLX
copyright and licenses. Human licensing and maintainers' acceptance remain
release/contribution gates.

Private control `tools/testing/check_arm64_mlx_headers.py` in the integration
workspace checks the exact two-file patch, original/patched Linux x86 object
byte identity, OHOS x86 compilation, the original OHOS ARM64 rejection and the
patched ARM64 declaration sizes. Undefining `__OHOS__` retains the original
ARM64 rejection with that same compiler. These are compiler controls, not
cross-platform runtime or numerical qualification.

ARM64 APP/CLI ELF construction alone is insufficient: target loader/runtime,
HAP packaging/signing, process supervision, actual inference and
Linux FP64-RoPE comparisons must still be qualified on the ARM64 PC. The current
Go route is Linux Go runtime plus OHOS C bindings, not an official native GOOS.

The private integration builder now also supports the explicit compile-only
`--native-only --arch arm64 --tls --router` probe. Five native CPU tools and
their Ollama-compatible SDK cross-link have been checked as AArch64 outputs;
they are not deployed or runtime-qualified and do not replace the independent
plain-upstream SDK. The SDK CMake ABI is `arm64-v8a`, not `arm64`.
Keep this separate from the header candidate and from the x86 product path.

The private `tools/testing/build_arm64_hap_probe.py` also assembles an unsigned
ARM64 HAP/HNP in an isolated source copy. It verifies every payload hash and
ELF architecture, refuses source links, and preserves the default x86 product
packager gate. The final probe has passed the existing native source-fingerprint,
debug-mode and CA-provider packaging contracts. It has not been signed,
installed or runtime-qualified. This closes only the unsigned cross-build
check; ARM64 inference, supervision, permissions and trusted release remain E6.

For an authorized E6 device session the private builder now accepts an explicit
`--purpose physical-candidate`. It rebuilds into a differently named unsigned
candidate and records eligibility for separate, bundle/device-bound signing.
The default compile-only output and all previous compile-only records remain
non-deployable; this switch is not runtime or release qualification.
