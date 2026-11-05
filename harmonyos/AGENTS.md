# HarmonyOS port maintenance

Keep only source, documentation and tests needed to build and run the HarmonyOS port and suitable for upstream review.
Preserve upstream behavior on other platforms, licenses and commit history.
Keep whole-project architecture, model/operator optimization, private experiments and historical session records outside this fork.
Dependencies are external: document official download/install links and required versions; do not commit SDKs, DDKs, installers or downloaded packages.
Keep generated binaries, HAP/HNP/OMC, weights, converted tensors, logs, traces and test-result receipts outside tracked source.
Use explicit toolchain, source, target and output arguments; no personal device or workspace paths.
Do not set RLIMIT_AS. Keep bounded execution and owned-process cleanup in device tests.
Compilation and host tests do not establish target numerical, permission or release qualification.
Do not submit upstream PRs automatically. Publishing this downstream fork does not imply upstream acceptance.
