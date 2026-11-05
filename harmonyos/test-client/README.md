# Independent HarmonyOS CLI/HNP test client

This app runs installed public HNP commands under its own application identity when the target has no interactive terminal. It checks argv parsing, separate stdout/stderr, UTF-8 display, stdin/EOF, cancellation and child cleanup. It is a test client, not a terminal or a replacement for Ollama.
Source contains the full HAP project, native fixture and launcher. No model, SDK, package or private template is included. The launcher does not impose a virtual-address-space cap.

From the repository root:

```bash
node --test harmonyos/test-client/tests/*.test.mjs
python3 harmonyos/tools/build_test_client.py --out /path/to/new-client-build \
  --tools /path/to/command-line-tools/bin --java /path/to/jdk17 \
  --sdk-root /path/to/sdk --arch x86_64
```

Use --arch arm64 for a separately compiled ARM64 test package. Outputs are unsigned and local. Sign/install the test app explicitly using your own tooling; install the matching public Ollama HNP runtime separately. No model is needed for the checked-in help/I/O cases.

```bash
HDC=/path/to/hdc python3 harmonyos/tools/run_hnp_automation.py \
  --target DEVICE_SERIAL --cases harmonyos/test-client/cases/harness-controls.json \
  --out /path/to/new-client-test-output
```

The automation uses the installed Ollama app to verify separate application ownership. Keep it running before testing. Inspect --help for transport options. Do not use the debug-domain checks to claim retail permission qualification.
