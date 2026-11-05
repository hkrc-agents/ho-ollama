# HarmonyOS Native service ownership

**Archived experiment (2026-10-23):** the current product uses the executable
lifeline/subreaper route. This directory is not a product or public patch
payload. `tools/build_native_service_so.py` requires explicit `--experimental-so`;
its disposable source rewrites shared runner/discovery code and is not portable
to default upstream builds. Do not copy those rewrites into an upstream PR.
The technical description below records the earlier experiment, not current
product requirements.

This component implements the new **current-HAP-session** registry and private
control plane. It is not yet the production Ollama process launcher. The
installed APP still uses its previously qualified executable path.

## Ownership and process creation

The HAP Native C++ layer creates APP/core/runner/standalone instances using
`OH_Ability_StartNativeChildProcess`. All are platform-managed children of the
HAP; logical `owner` instance IDs retain the APP → core → runner dependencies.
The system relationship need not equal the kernel PPID. No ArkTS creation proxy,
fork/exec fallback, process-group cleanup, old-PID adoption, or boot recovery is
implemented here. An error from the official API stays an error.

`registry.{h,cpp}` contains the ownership/state machine. `ohos_platform.cpp`
is compiled only for OHOS and calls the actual official start/kill/callback APIs.
The host tests inject a platform model; the independent simulator fixture uses
the same registry and the real backend with bounded fake SO services.

An instance records its HAP session, role, owner, monotonic ID, PID/start ticks,
desired intent, state, replacement generation and exit proof. The 64-hex session
identifier is not a secret. Lifecycle JSONL omits launch payloads and credentials.
The log is an audit trail, not authority to recover a process after a HAP restart.
The in-memory registry is bounded to 4096 generations per session; exhaustion
rejects creation visibly instead of silently reusing identities.

## Cleanup and replacement

An owner's exit cancels its descendants' restart intent. `Pump()` uses the
official kill API in reverse dependency order and does not consider successful
kill submission an exit acknowledgement. Replacement waits for the old tree's
exit proof, uses a fresh instance, and never reuses its old runners. An owner
dying during native creation also causes that newly created instance to stop.

`CancelIntent()` is separate from `AbortTree()`: cancelling intent must not
replace the normal HTTP/storage/service drain protocol with forced termination.
`supervisor.{h,cpp}` supplies the HAP-side bounded restart owner for the top-level
Go APP only: default 1/2/4-second backoff, at most three restarts until a 30-second
stable run resets the budget. `RequestQuit()` cancels pending retries without
forcing normal shutdown. The existing Go APP retains core/standalone policy;
there is no second native loop competing to restart core. Neither layer silently
retries ambiguous create requests or tries to implement an entire systemd.

The simulator revealed an actual missing callback after a complete child-host
turnover. Re-registering the callback did not fix it. For **already registered,
officially created instances only**, the HAP can independently confirm a missing
`/proc/<PID>` directory. Only ENOENT/ESRCH establishes `proc_absent`; permission
errors, unreadable files, and identity mismatches remain unknown. This is neither
an orphan scan nor PID adoption. Audit records distinguish that proof from an
official callback, and never invent a signal/exit code. A callback for an identity
that is demonstrably still live cannot retire a new generation.

`Drained(id)` covers that generation and its logical descendants. User Stop/Quit
must also check `Quiescent(id)`, including any replacement reserved concurrently
with the stop. Recovery must not replay the same replacement generation twice.

## Native control plane and Go client

`control_server.{h,cpp}` implements bounded IPv4-loopback, length-framed control
requests directly in Native C++; there is no shell or ArkTS dispatch. Two request
workers and one cleanup pump are bounded; partial clients cannot block shutdown
indefinitely. The server needs the HAP's normal INTERNET permission when used on
the device. Its actual target networking is not qualified by a host socket test.
A separate emulator test passed the real official C++ SO → private TCP → HAP
Native → official runner creation path. This uses bounded fake services, not
the real Go APP/core or a model inference session.

The private per-session capability must differ from the logged session ID. It
belongs in private SO bootstrap data, never logs or public CLI configuration.
Registry checks additionally enforce live caller and logical owner boundaries.
This protection is **only for the internal process-management API**, not a new
authentication requirement on Ollama/OpenAI HTTP or public HNP clients. Trusted
managed components share this private capability; it is not UID authentication.

`go/` is the `harmonyos`-tagged client, with no non-standard dependencies. It has
explicit cancellation, bounded frames, identity validation, generation drain vs
quiescence waits, and actual Go-to-C++ host interoperability tests. It does not
pretend to be `os.Process`/`exec.Cmd`, manufacture exit codes from callback signals,
or automatically retry a possibly successful create after a transport failure.

The following integration is **still required before product promotion**:

1. Carry private endpoint/capability and the instance/session into each real SO
   bootstrap; connect the HAP's Native launch/stop/token/status entry points.
2. Build/package actual Go APP, core and matched runner/standalone SOs, using the
   private GD-TLS toolchain where required; validate installed hashes before use.
   Include internal model-store/storage-probe helpers as official worker SOs;
   moving only the four long-lived roles does not satisfy the no-exec service tree.
3. Adapt upstream process handles and readiness to managed ownership, rather
   than relying on `waitpid`, executable argv/PGID, or direct-child PPID checks.
   Preserve the original restart policy and normal drain/storage/stop ACKs.
4. Define create delivery/timeout reconciliation before integrating retries; a
   cancelled TCP request must not cause an unreported duplicate model load.
5. Run real model, storage, background/tray/Quit and complete-tree crash regression.
   Keep existing qualified binaries until those gates pass. ARM64/physical-device
   qualification is separate, not a substitute for missing engineering work.

## Reproducible checks

```bash
python3 scripts/verify_native_service_host.py --out artifacts/<fresh-host-run>
python3 scripts/build_managed_native_services_probe.py --out artifacts/<fresh-build>
python3 scripts/verify_managed_native_services_probe.py \
  --build artifacts/<fresh-build> --out artifacts/<fresh-device-run>
```

The host gate runs Release and ASan/UBSan cases with assertions enabled, Go race
tests including Go/C++ interoperability, default-Linux exclusion and real OHOS
header compilation. Repeated/sanitizer events are not additional test identities.
The device gate checks actual installed library hashes, resource limits, core and
APP crash cleanup/replacement, cooperative stop, SO-to-HAP runner creation, and HAP self-SIGKILL. HAP-death
proof must be captured before any HDC force-stop and before fixture self-expiry.
Those are real management tests with fake services, **not full-model acceptance**.
