# HarmonyOS application host

ArkWeb hosts the upstream React UI at pages/OfficialIndex.ets. The Go APP owns chat and SQLite services; ArkTS owns windows, tray, system pickers and platform permissions. Executable services use the parent-lifeline/subreaper supervision path.

Build from the repository root with `harmonyos/tools/build_hap.py` and explicit
`--tools-dir`, `--java-home`, `--sdk-root` and `--out` arguments. See
[the HarmonyOS build guide](../../HARMONYOS.md). The normal build installs
dependencies in a staged project under `--out` and exports the unsigned HAP
and input fingerprints there.
Use harmonyos/tools/package_full_hap.py with the freshly built runtime to create the complete unsigned HAP. Personal signing and device selection are external.
Debug-only diagnostic requests are disabled in release. Test scripts and code must preserve that separation. No SDK, model, installer, runtime binary or test-result log belongs in this directory.
