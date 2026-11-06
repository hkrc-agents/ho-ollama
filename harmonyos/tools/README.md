# HarmonyOS build and test tools

See ../../HARMONYOS.md for complete commands and external dependency links.

- build_runtime.py builds the matched native CPU tools and Go CLI from pinned upstream Git objects plus the owned platform patches. Pass the llama.cpp checkout and SDK explicitly. --prepare-only creates isolated source trees without compiling. --tls enables LibreSSL and the explicit CA-directory adapter; --router enables the upstream subprocess route.
- prepare_official_app.py installs the maintained Go/React port into the pinned source or its downstream descendant. Keep common platform-independent behavior unchanged.
- build_official_app.py validates the input payload, rebuilds the frontend before Go embedding, compiles the APP/CLI and native helpers, and writes local identities.
- build_hap.py stages the HAP project under `--out`, builds it with explicit SDK, Java and command-line tools, and exports the unsigned HAP there.
- package_full_hap.py creates the complete HNP/HAP from fresh hash-checked outputs. hap_contract.py validates archives and the executable runtime contract.
- build_test_client.py builds the independent CLI/HNP test HAP. run_hnp_automation.py submits bounded argv/I/O requests through it, using an explicitly selected HDC target.
- run_app_host_tests.py builds the required real subprocess fixtures, runs the APP/server race suite and portable contracts. It requires a prepared source with real frontend assets.
- verify_harmonyos_go_test_portability.py and audit_harmonyos_go_test_portability.py exercise desktop test fixture portability; confined_exec.py supplies Linux host-test confinement.

Generated packages, logs and result JSON are local outputs, not source files. These tools do not install applications, sign releases or qualify device inference as part of ordinary compilation.
