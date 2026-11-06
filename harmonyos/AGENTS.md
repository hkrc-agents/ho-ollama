# HarmonyOS port maintenance

Keep only source, documentation and tests needed to build and run the HarmonyOS port and suitable for upstream review.
Preserve upstream behavior on other platforms, licenses and commit history.
Keep whole-project architecture, model/operator optimization, private experiments and historical session records outside this fork.
Dependencies are external: document official download/install links and required versions; do not commit SDKs, DDKs, installers or downloaded packages.
Keep generated binaries, HAP/HNP/OMC, weights, converted tensors, logs, traces and test-result receipts outside tracked source.
Put new build and test outputs in fresh subdirectories of the repository's Git-ignored `build/` or `target/`.
Use explicit toolchain, source, target and output arguments; no personal device or workspace paths.
Do not set RLIMIT_AS. Keep bounded execution and owned-process cleanup in device tests.
Use pinned source objects and fresh staging for runtime and HAP builds. Check the actual native build mode and package hash before reusing build or release evidence.
Serialize device tests. Record architecture, OS, caller UID/domain and staged hashes. Distinguish emulator and physical results, batch HDC and interactive PTY, and app or public HNP permission checks from privileged controls.
Treat compilation, host tests, target permissions and retail signing as separate results. Report failures, skips and unexecuted cases separately.
Do not submit upstream PRs automatically. Publishing this downstream fork does not imply upstream acceptance.

## Application contracts

- Keep window hide and tray Quit separate. Quit must wait for service drain, storage rollback where needed and verified stop acknowledgements. Preserve the host if shutdown fails.
- Follow the [service launch designs](SERVICE_LAUNCH_DESIGNS.md) for process ownership. Recovery acts only on verified descendants of the current session and stops when ownership or cleanup is uncertain.
- Keep the default private model store usable. Custom storage requires persistent picker authorization and transactional migration. Surface authorization failure to the user so they can choose how to recover.
