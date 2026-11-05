# HarmonyOS downstream maintenance

This directory owns the existing CPU HarmonyOS port and its portable build/test tools.
Preserve upstream source history, licenses, and platform behavior outside HarmonyOS.
Keep the tested upstream base distinct from the downstream revision and actual binary hashes.
Use explicit SDK, compiler, output and cross-repository source arguments. Never require the old pico checkout.
Keep generated files, SDKs, signing keys, device identities and model weights out of Git.
Do not add the fused NPU model or its compiler/runtime to this CPU port.
Do not set RLIMIT_AS or request an exception for it; that limit was permanently removed.
Build/host-test passes do not imply device numerical, lifecycle or release qualification.
No device deployment, signing, reboot or user-setting change is part of repository preparation.
Upstream contributions are a separate human-reviewed activity; do not open PRs automatically.

Keep Ascend C full-network operator construction and optimization in the private ho-llama-models repository. Future public llama.cpp changes may load/invoke a released operator only after correctness and matched performance qualification. Do not copy private sources/build recipes into this fork.
