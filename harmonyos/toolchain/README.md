# Private Go 1.26 amd64/OHOS dynamic TLS diagnostic

This is an opt-in experimental toolchain patch, not a new default Go runtime,
product dependency, or released HarmonyOS GOOS. Keep the installed stock Go
tree untouched. The actual candidate compiler and build logs belong in fresh
private ext4 storage; this directory preserves the reviewable source delta.

## Source and license

The reference is Jay Gowdy's unmerged work linked from Go issue 71953:

- `3d32f2325173c738d8b0c2053cf051d4cd13b173`
- `d550c0d6d72dae5abea357a905e72ec2e2d9aa36`
- `6ea22ce797552d59bb2c303516c00e503bb22ed1`
- [selected reference `1a087d05b5cf9573876b18812d8d5516f16bbe57`](https://github.com/jgowdy/go/commit/1a087d05b5cf9573876b18812d8d5516f16bbe57)

These are alternative revisions of the same change, not four commits to apply
in sequence. The reference spans 45 files/multiple architectures and assumes
newer runtime/cgo source organization. Do not apply the whole fork to Go 1.26.
The diagnostic preserves only the necessary amd64/toolchain path and replaces
the inline push/call/pop encoder with explicit Go Progs.

## R2 leaf-frame correction (2026-10-22)

R2 is the preserved leaf-frame revision; the canonical patch is now R3 (below).
The frozen R1 adapted patch is preserved at
`/home/developer/.cache/pico/go-gd-tls-review-20261022-5V2tCX/go1.26.0-ohos-amd64-gd-tls-r1.patch`
(SHA256 `c408a5525264c95cb58d00aeee439c97cbf5995c7461b7537d16054d3437ed0c`).
R1 source/binaries and failed runs are not overwritten.

The only R2 compiler-source delta from R1 is in
`src/cmd/internal/obj/x86/obj6.go`: `preprocess` ignores
`q.As == obj.ACALL && q.Mark&tlsGDUnsafe != 0` in its `hasCall` scan.
The marked TLSDESC call implements an original MOV, not a Go call. Counting
it as a Go call adds a BP frame to originally frameless zero-local assembly.
Stack-swap/tail-jump runtime code can then operate with an unexpected frame;
corrupted return/context restoration is the crash hypothesis, not a claim
that every observed crash has been instruction-by-instruction explained.
Real unmarked calls retain their original frame behavior. The explicit TLS
push/pop/call Progs, Spadj/PCSP and asynchronous-unsafe marking remain intact.

Frozen R2 compiler:
`/home/developer/.cache/pico/go-gd-tls-r2-20261022-cCYv4b/go1.26-ohos-gd-amd64/bin/go`.
Only `cmd/asm` and `cmd/compile` were rebuilt, with `GOMAXPROCS=4`,
`GOFLAGS=-p=4`, `GOTOOLCHAIN=local`, and explicit candidate `GOROOT`.
`go install cmd/asm cmd/compile` exited 0; an empty `rebuild-tools.log`
means no diagnostics, not an unfinished build. `bin/go`, linker and cgo are
the frozen R1 binaries copied to the fresh ext4 tree; the R1 full bootstrap
remains their provenance. R2 identity/log/audit files live beside that tree,
not inside its frozen source directory.

Hubble's `artifacts/harmonyos_go_dynamic_tls_20261022-r1/gd-glibc-r2/identity.json`
records `host_pass`; the R1 GD/glibc SIGSEGV is preserved separately. A local
read-only ELF audit (`leaf-pcsp-audit.json` beside the R2 tree) confirms no
automatic BP push in `gogo` and `tlscheck.Check`, correct TLS push/pop PCSP,
and unsafe PCDATA, including the callback TLS sequence. This is bounded A/B
support for the frame hypothesis, not generic glibc resolver, signal/unwind,
OHOS service or product qualification. Fixture hashes/commands and the R2
identity must be compared before treating a workload as an exact control.
The recorded fixture-source comparison finds all R1 source files unchanged;
R2 adds only `tests/test_asm_frames.py` (a host disassembly assertion, not
linked fixture code). A recursive comparison of the complete compiler `src/`
trees finds only the stated `obj6.go` delta. Paths/build IDs and final binary
hashes differ and are not represented as byte-identical controls.

R2 host unit run exited 0: `cmd/internal/obj/x86` and `cmd/internal/objabi`
passed; `cmd/link/internal/amd64` reports skip/no test files (not a pass).
`identity.json` beside the R2 tree records stock version/hash, all adapted
source hashes, exact R1/R2 patch hashes, actual tool hashes, inherited full
bootstrap and R2 rebuild commands/exit/log hashes, fixture comparison and
the emitted-code/PCSP audit. The canonical patch applies cleanly to stock
via `git apply --check`; neither this check nor these tests edit the compiler.

## R3 process argv initialization (2026-10-22)

Frozen compiler:
`/home/developer/.cache/pico/go-gd-tls-r3-20261022-AEhjJo/go1.26-ohos-gd-amd64/bin/go`.
Its go/asm/compile/link/cgo binaries are byte-identical to R2. R3 changes only
`runtime1.go` and the two `pico_ohos_dynamic_tls*` runtime files; no bootstrap
or tool rebuild is required. Dependent builds recompile runtime in their cache.
The R2 patch snapshot is preserved beside the R2 tree as
`go1.26.0-ohos-amd64-gd-tls-r2.patch`, SHA256
`03f5a1e30b89c3f3b2edfb5bbfb4763a4bc9c529e057c04a8a3d29b9dc1c3ffa`.
The canonical patch now applies R3 directly to stock; the separate
`go1.26.0-ohos-amd64-gd-tls-r3-argv.patch` is incremental **after R2 only**.
Do not apply both to stock or reapply the incremental patch to R3.

The old opt-in `goargs` produced an empty slice. The actual APP device log
`actual-app-help-device-r2/go-help-stdio.log` records an index-0/length-0 panic.
The prepared APP pins pflag v1.0.5; its `flag.go:1212` initializes
`CommandLine = NewFlagSet(os.Args[0], ExitOnError)`. This is a concrete
package-init failure path, but the truncated panic has no stack identifying
the exact package. Native `before_actual_Main` proves neither entry into the
Go export nor completion of Go package initialization. Require Main's actual
help-output gate (`--require-go-help`) for the APP probe.

Under the existing opt-in Linux/amd64 library/archive guard, R3 reads
`/proc/self/cmdline` immediately in `goargs` (before `goenvs`), retries EINTR,
uses 4096-byte chunks and a 1 MiB bound, and copies strings into `argslice`.
Empty/missing/unreadable/oversized cmdline keeps `pico-go-so` as argv[0]; an
empty first argument is replaced with that name while other arguments survive.
It never trusts the non-glibc constructor's argc/argv. This is **host process
identity**, not an HNP/package executable path, not replacement for application
entry JSON argv/env, and not a fix for all package-init resource paths.
Reading proc environ still cannot promise to observe a C `setenv` performed
after exec; exported-entry configuration arrives too late for package init.

`artifacts/harmonyos_go_dynamic_tls_20261022-r1/toolchain-r3/identity.json`
binds all 16 adapted source files, stock/R2/R3 tool hashes, patch snapshots,
inherited bootstrap/rebuild commands, exact probe build command/output and logs.
Patch reconstruction in a fresh private source subset matches all 16 frozen
R3 files. Local late-dlopen tests pass actual proc argv (Unicode and empty
arguments), empty argv[0] fallback, package-level args/flag/env initialization,
and argv survival across a 16 KiB environment read. Proc open/read failure
and the oversized-cmdline fallback have not been directly injected/tested.
Hubble's GD/profile/init and default-IE/profile/init identities report
`host_pass`; OHOS `built` is not device success. Actual APP and device/profile
results remain separate, identity-bound gates; no product SO is qualified here.

R3's copied, parameterized `toolchain-r3/audit_leaf_pcsp.py --so <actual.so>`
also passed on the actual `gd-glibc-r3-profile-init` and
`gd-ohos-r3-profile-init` ELFs. `leaf-pcsp-glibc.json` and
`leaf-pcsp-ohos.json` bind ELF and audit-source SHA256 and verify gogo/Check
remain without auto BP frames, plus tracked TLS push/pop PCSP and unsafe
PCDATA in those functions and cgocallback. GNU objdump emits DWARF-form
warnings for the OHOS ELF; raw stderr is retained, and the actual instruction
and pclntab assertions passed. This static audit does not prove arbitrary
signal unwinding inside a loader resolver.

## Rebuild from stock in a fresh tree

`build_private_go.py` copies the pinned stock tree into a newly created ext4
directory, applies the canonical R3 patch, runs `src/make.bash` with the stock
bootstrap and at most four jobs, and writes source/tool/patch hashes, command,
environment, exit and logs outside the new compiler tree. It never edits the
stock tree or defaults; there is no automatic device install or product change.

```bash
python3 repos/ollama/harmonyos/toolchain/build_private_go.py \
  --stock /home/developer/.cache/pico/toolchains/go1.26 \
  --private-parent /home/developer/.cache/pico --jobs 4
```

The wrapper's help/syntax and source-patch reconstruction have been checked;
a new full bootstrap through this wrapper has not been run. Original R1 full
bootstrap and R2 tool rebuild logs remain the current binary provenance.

Go's [LICENSE at the reference](https://github.com/jgowdy/go/blob/1a087d05b5cf9573876b18812d8d5516f16bbe57/LICENSE)
is BSD-3-Clause. Retain that license, existing per-file notices (including
the x86 backend's historical permissive notices), and the reference attribution.
The patch is an adapted downstream experiment, not an upstream-approved fix.

## Explicit enablement

Build the private compiler with the stock compiler as GOROOT_BOOTSTRAP,
GOMAXPROCS=4, and at most four concurrent package jobs. No GD flag is enabled
while bootstrapping. Ordinary builds with this compiler retain stock IE/LE.

For the Linux/amd64 runtime with OHOS C bindings, opt in explicitly:

```text
-buildmode=c-shared
-tags=harmonyos,netcgo,pico_ohos_dynamic_tls
-asmflags=all=-tls=GD
-gcflags=all=-tls=GD
```

Keep GOOS=linux, GOARCH=amd64, CGO_ENABLED=1 and the existing OHOS clang/sysroot.
Use an isolated build cache and record actual compiler tools/source/output
hashes. Do not infer runtime qualification from compilation or relocations.
The extra runtime tag avoids trusting non-glibc constructor argc/argv, reads
auxv/environment via procfs, and leaves application argv/config to its exported
entry. It does not repair application package-init paths derived before Main.

## Safety boundary

- GD lowering is selected explicitly, never forced in cmd/go defaults.
- A MOV TLS expands before preprocessing into PUSHFQ, optional PUSHQ AX, a
  descriptor-address Prog, an explicit indirect CALL, optional MOV/POP AX,
  and POPFQ. Stack changes therefore participate in Spadj/PCSP generation.
- The descriptor-address encoder emits only LEA and its TLS relocations;
  it contains no hidden push/pop/call. The generated sequence and subsequent
  TLS dereference are marked unsafe for asynchronous preemption.
- RAX and flags must survive exactly as an original MOV requires.
  [OHOS musl's public amd64 resolver](https://github.com/openharmony/third_party_musl/blob/master/src/ldso/x86_64/tlsdesc.s)
  saves RCX/RDX but modifies flags through ADD/SUB. Its dynamic path is short
  assembly and makes no C calls; it does not require a SysV C slow-path stack
  alignment. This is a bounded-musl hypothesis until the actual device loader
  identity/instructions and signal/unwind behavior are verified.
- This code does not switch to g0/C stack. Generic glibc lazy resolution,
  arbitrary loader implementations, profiling/signals inside the resolver,
  runtime resource limits and production concurrency remain independent gates.
  See the stack/signal discussion in [Go issue 71953](https://github.com/golang/go/issues/71953).

Required evidence includes actual emitted instructions/PCSP/unsafe PCDATA,
live-AX and arithmetic-flags tests, default-build control, native foreign-thread
callbacks, signal/profile/unwind stress and the real OHOS official-child load.
None of these may be replaced by an author claim about ARM64 Alpine tests.
