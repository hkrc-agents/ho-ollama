# Import validation (2026-11-04)

Passed on Linux/WSL with external official HarmonyOS API23 SDK, Go 1.26.0, Node 24 and JDK 17:

- 279 frontend tests, no failures/skips; real route generation and production React build.
- APP/server host race suite: 319 pass events including subtests, both packages passed.
- 22 portable contract tests for ELF supervision, unchanged-platform wait behavior, storage authorization and fresh packaging; the five fresh-packaging cases also passed after the final required-CLI list update.
- Native service registry: 24 Release cases and 24 ASan/UBSan cases; Go race client: 14 parent tests and 21 subtests; four source files compiled against real OHOS headers.
- Clean matched CPU runtime build: native CLI/server/bench/quantize/simple, SDK archives/headers and OHOS-linked Go CLI.
- Full Go APP build embeds the freshly rebuilt React distribution and compiles its native launcher/storage helper.
- Unsigned HAP built from the owned HAP source, with unchanged input fingerprints.
- The new full packager created a new five-link HNP without a historical base HAP; SDK HAP content and every packaged runtime file were hash-verified.

Exact binary/HAP hashes are in VALIDATION.json. SDKs, generated packages, weights and signing material are not committed.
The tested minimal native runtime has TLS/router disabled. Those opt-in recipes and adapters are retained but not requalified by this build.
No new device, ARM64 Go, model numerical, lifecycle or retail-release acceptance is claimed.

Initial validation attempts exposed an old Node 12 environment, missing host fixtures/CGO setup,
and a test's old Go cache path. Portable entry points now select explicit toolchains, build their
fixtures and run the nonempty suites; failures were corrected rather than counted as passes.
