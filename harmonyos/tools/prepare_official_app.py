#!/usr/bin/env python3
"""Enable upstream platform-neutral APP packages; install small OHOS adapters.

Only the pinned disposable source checkout is changed, never vendor/llama.cpp.
The build-tag edit is mechanical; upstream React and API implementations stay intact.
"""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

ADAPTER = Path(__file__).resolve().parents[1]
PIN = "8a971df3bacec93944ec894f2939fd2567afc5dd"

def prepare_think_false(source):
    """Apply only the scoped fix; caller holds runtime-build.lock, as for main()."""
    if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip() != PIN and subprocess.run(["git", "merge-base", "--is-ancestor", PIN, "HEAD"], cwd=source, capture_output=True).returncode != 0:
        raise SystemExit("Refusing unpinned upstream source")
    patch = ADAPTER/"app-think-false.patch"
    test = ADAPTER/"ui_think_harmony_test.go"
    target = source/"app/ui"/test.name
    if target.exists() and target.read_bytes() != test.read_bytes():
        raise SystemExit("Refusing to overwrite modified think regression test")
    if subprocess.run(["git", "apply", "--reverse", "--check", str(patch)], cwd=source, capture_output=True).returncode != 0:
        if subprocess.run(["git", "apply", "--check", str(patch)], cwd=source, capture_output=True).returncode != 0:
            raise SystemExit("Think false patch does not match pinned source; preserving local changes")
        subprocess.run(["git", "apply", str(patch)], cwd=source, check=True)
    if not target.exists():
        shutil.copyfile(test, target)
    return {
        "think_false_patch_sha256": hashlib.sha256(patch.read_bytes()).hexdigest(),
        "think_false_test_sha256": hashlib.sha256(target.read_bytes()).hexdigest(),
    }

def prepare_portable_tests(source):
    """Fix mock/test platform assumptions, never production update behavior."""
    if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip() != PIN and subprocess.run(["git", "merge-base", "--is-ancestor", PIN, "HEAD"], cwd=source, capture_output=True).returncode != 0:
        raise SystemExit("Refusing unpinned upstream source")
    patch = ADAPTER/"test-fixture-portability.patch"
    rows = subprocess.check_output(["git", "apply", "--recount", "--numstat", str(patch)], cwd=source, text=True)
    paths = {row.split("\t")[-1] for row in rows.splitlines()}
    if paths != {"app/ui/ui_test.go", "cmd/config/config_test.go", "app/updater/updater_test.go"}:
        raise SystemExit("Test portability patch escaped its three test files")
    if subprocess.run(["git", "apply", "--recount", "--reverse", "--check", str(patch)], cwd=source, capture_output=True).returncode != 0:
        subprocess.run(["git", "apply", "--recount", "--check", str(patch)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--recount", str(patch)], cwd=source, check=True)
    return hashlib.sha256(patch.read_bytes()).hexdigest()

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, default=Path.home()/".cache/ho-ollama/sources/ollama")
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    source = args.source.resolve()
    if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip() != PIN and subprocess.run(["git", "merge-base", "--is-ancestor", PIN, "HEAD"], cwd=source, capture_output=True).returncode != 0:
        raise SystemExit("Refusing unpinned upstream source")
    think_evidence = prepare_think_false(source)
    # The final correction changes lines also used by historical reverse
    # checks. Remove only our exact correction before replaying that chain.
    isolation_patch = ADAPTER/"ui-platform-isolation.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(isolation_patch)], cwd=source, capture_output=True).returncode == 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", str(isolation_patch)], cwd=source, check=True)
    patch = ADAPTER/"app-onboarding-path.patch"
    if subprocess.run(["git", "apply", "--check", str(patch)], cwd=source, capture_output=True).returncode == 0:
        subprocess.run(["git", "apply", str(patch)], cwd=source, check=True)
    elif subprocess.run(["git", "apply", "--reverse", "--check", str(patch)], cwd=source, capture_output=True).returncode != 0:
        raise SystemExit("Onboarding path patch does not match pinned source")
    ui_patch = ADAPTER/"ui-platform.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(ui_patch)], cwd=source, capture_output=True).returncode == 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(ui_patch)], cwd=source, check=True)
    elif subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(ui_patch)], cwd=source, capture_output=True).returncode != 0:
        raise SystemExit("UI platform patch does not match pinned source")
    shutil.copyfile(ADAPTER/"platform.harmony.test.ts",
                    source/"app/ui/app/src/lib/platform.harmony.test.ts")
    navigation_patch = ADAPTER/"ui-model-navigation.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(navigation_patch)], cwd=source, capture_output=True).returncode == 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(navigation_patch)], cwd=source, check=True)
    elif subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(navigation_patch)], cwd=source, capture_output=True).returncode != 0:
        raise SystemExit("Model navigation patch does not match pinned source")
    shutil.copyfile(ADAPTER/"ui/models.tsx", source/"app/ui/app/src/routes/models.tsx")
    shutil.copyfile(ADAPTER/"ui/models.test.tsx", source/"app/ui/app/src/lib/models.test.tsx")
    for name in ("model-download.ts", "model-download.test.ts", "model-import.ts", "model-import.test.ts", "window-layout.ts", "window-layout.test.tsx"):
        shutil.copyfile(ADAPTER/"ui"/name, source/"app/ui/app/src/lib"/name)
    window_patch = ADAPTER/"ui-window-layout.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(window_patch)], cwd=source, capture_output=True).returncode == 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(window_patch)], cwd=source, check=True)
    elif subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(window_patch)], cwd=source, capture_output=True).returncode != 0:
        raise SystemExit("Window layout patch does not match pinned source")
    capability_patch = ADAPTER/"ui-capabilities.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(capability_patch)], cwd=source, capture_output=True).returncode == 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(capability_patch)], cwd=source, check=True)
    elif subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(capability_patch)], cwd=source, capture_output=True).returncode != 0:
        raise SystemExit("Native capability patch does not match pinned source")
    startup_patch = ADAPTER/"ui-login-startup.patch"
    startup_status_patch = ADAPTER/"ui-login-startup-status.patch"
    status_applied = subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(startup_status_patch)], cwd=source, capture_output=True).returncode == 0
    if status_applied:
        pass  # v2 replaces the v1 block; do not reinsert the old block.
    elif subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(startup_patch)], cwd=source, capture_output=True).returncode == 0:
        pass  # An insertion-only patch can also pass a forward check twice.
    elif subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(startup_patch)], cwd=source, capture_output=True).returncode == 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(startup_patch)], cwd=source, check=True)
    elif subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(startup_patch)], cwd=source, capture_output=True).returncode != 0:
        raise SystemExit("Login startup patch does not match pinned source")
    if not status_applied:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(startup_status_patch)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(startup_status_patch)], cwd=source, check=True)
    deduplicate_patch = ADAPTER/"ui-login-startup-deduplicate.patch"
    if subprocess.run(["git", "apply", "--recount", "--check", str(deduplicate_patch)], cwd=source, capture_output=True).returncode == 0:
        subprocess.run(["git", "apply", "--recount", str(deduplicate_patch)], cwd=source, check=True)
    settings_source = (source/"app/ui/app/src/components/Settings.tsx").read_text()
    if settings_source.count("<HarmonyLoginStartup />") != 1 or "Check the actual system status before enabling." in settings_source:
        raise SystemExit("Login startup section must appear exactly once, without the legacy card")
    for name in ("login-startup.tsx", "login-startup.test.tsx"):
        shutil.copyfile(ADAPTER/"ui"/name, source/"app/ui/app/src/lib"/name)
    service_patch = ADAPTER/"ui-services.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(service_patch)], cwd=source, capture_output=True).returncode != 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(service_patch)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(service_patch)], cwd=source, check=True)
    if (source/"app/ui/app/src/components/Settings.tsx").read_text().count("<HarmonyServices />") != 1:
        raise SystemExit("Exactly one platform service management section required")
    for name in ("services.tsx", "services.test.tsx"):
        shutil.copyfile(ADAPTER/"ui"/name, source/"app/ui/app/src/lib"/name)
    storage_patch = ADAPTER/"ui-storage.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(storage_patch)], cwd=source, capture_output=True).returncode != 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(storage_patch)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(storage_patch)], cwd=source, check=True)
    for name in ("model-export.tsx", "model-storage.tsx", "model-storage-transfer.test.tsx"):
        shutil.copyfile(ADAPTER/"ui"/name, source/"app/ui/app/src/lib"/name)
    changed = []
    for package in ("ui", "store", "server", "updater", "version", "tools", "logrotate", "auth", "types/not"):
        for path in (source/"app"/package).rglob("*.go"):
            original = path.read_text()
            before = "//go:build windows || darwin\n"
            after = "//go:build windows || darwin || harmonyos\n"
            if original.startswith(before):
                path.write_text(after + original[len(before):])
            if original.startswith((before, after)):
                changed.append(str(path.relative_to(source)))
    core_output_patch = ADAPTER/"core-output.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(core_output_patch)], cwd=source, capture_output=True).returncode != 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(core_output_patch)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(core_output_patch)], cwd=source, check=True)
    model_root_patch = ADAPTER/"core-model-root.patch"
    core_wait_patch = ADAPTER/"core-wait.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(core_wait_patch)], cwd=source, capture_output=True).returncode != 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(core_wait_patch)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(core_wait_patch)], cwd=source, check=True)
    if subprocess.run(["git", "apply", "--recount", "--reverse", "--check", str(model_root_patch)], cwd=source, capture_output=True).returncode != 0:
        subprocess.run(["git", "apply", "--recount", "--check", str(model_root_patch)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--recount", str(model_root_patch)], cwd=source, check=True)
    root_tests = ADAPTER/"core-model-root-tests.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(root_tests)], cwd=source, capture_output=True).returncode != 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(root_tests)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(root_tests)], cwd=source, check=True)
    for name, dest in (("main.go", "app/cmd/harmony/main.go"),
                       ("command_path.go", "app/cmd/harmony/command_path.go"),
                       ("command_path_test.go", "app/cmd/harmony/command_path_test.go"),
                       ("runtime_layout_harmony.go", "app/cmd/harmony/runtime_layout_harmony.go"),
                       ("runtime_layout_exec_harmony.go", "app/cmd/harmony/runtime_layout_exec_harmony.go"),
                       ("runtime_layout_harmony_test.go", "app/cmd/harmony/runtime_layout_harmony_test.go"),
                       ("runtime_managed_harmony.go", "app/cmd/harmony/runtime_managed_harmony.go"),
                       ("runtime_diagnostics_disabled.go", "app/cmd/harmony/runtime_diagnostics_disabled.go"),
                       ("runtime_diagnostics_enabled.go", "app/cmd/harmony/runtime_diagnostics_enabled.go"),
                       ("core_managed_harmony.go", "app/cmd/harmony/core_managed_harmony.go"),
                       ("server_managed_harmony.go", "app/cmd/harmony/server_managed_harmony.go"),
                       ("worker_managed_harmony.go", "app/cmd/harmony/worker_managed_harmony.go"),
                       ("storage_probe_managed_harmony.go", "app/cmd/harmony/storage_probe_managed_harmony.go"),
                       ("managed_store_paths.go", "app/store/managed_paths_harmony.go"),
                       ("managed_server_paths.go", "app/server/managed_paths_harmony.go"),
                       ("shutdown_harmony.go", "app/cmd/harmony/shutdown_harmony.go"),
                       ("shutdown_harmony_test.go", "app/cmd/harmony/shutdown_harmony_test.go"),
                       ("models_harmony.go", "app/cmd/harmony/models_harmony.go"),
                       ("models_harmony_test.go", "app/cmd/harmony/models_harmony_test.go"),
                       ("gguf_harmony.go", "app/cmd/harmony/gguf_harmony.go"),
                       ("gguf_harmony_test.go", "app/cmd/harmony/gguf_harmony_test.go"),
                       ("gguf_integration_test.go", "app/cmd/harmony/gguf_integration_test.go"),
                       ("model_store_worker_harmony.go", "app/cmd/harmony/model_store_worker_harmony.go"),
                       ("model_root_harmony.go", "app/cmd/harmony/model_root_harmony.go"),
                       ("model_root_authorization_harmony_test.go", "app/cmd/harmony/model_root_authorization_harmony_test.go"),
                       ("storage_manager_harmony.go", "app/cmd/harmony/storage_manager_harmony.go"),
                       ("storage_manager_harmony_test.go", "app/cmd/harmony/storage_manager_harmony_test.go"),
                       ("storage_migration_harmony.go", "app/cmd/harmony/storage_migration_harmony.go"),
                       ("storage_migration_harmony_test.go", "app/cmd/harmony/storage_migration_harmony_test.go"),
                       ("core_models_harmony_test.go", "app/server/core_models_harmony_test.go"),
                       ("gguf_recovery_harmony.go", "app/cmd/harmony/gguf_recovery_harmony.go"),
                       ("gguf_recovery_harmony_test.go", "app/cmd/harmony/gguf_recovery_harmony_test.go"),
                       ("core_models_harmony.go", "app/server/core_models_harmony.go"),
                       ("core_models_default.go", "app/server/core_models_default.go"),
                       ("downloads_harmony.go", "app/cmd/harmony/downloads_harmony.go"),
                       ("downloads_harmony_test.go", "app/cmd/harmony/downloads_harmony_test.go"),
                       ("services_harmony.go", "app/cmd/harmony/services_harmony.go"),
                       ("services_harmony_test.go", "app/cmd/harmony/services_harmony_test.go"),
                       ("supervision_harmony.go", "app/cmd/harmony/supervision_harmony.go"),
                       ("supervision_harmony_test.go", "app/cmd/harmony/supervision_harmony_test.go"),
                       ("storage_probe_harmony.go", "app/cmd/harmony/storage_probe_harmony.go"),
                       ("storage_probe_harmony_test.go", "app/cmd/harmony/storage_probe_harmony_test.go"),
                       ("core_control_harmony.go", "app/cmd/harmony/core_control_harmony.go"),
                       ("core_control_harmony_test.go", "app/cmd/harmony/core_control_harmony_test.go"),
                       ("server_harmony.go", "app/server/server_harmony.go"),
                       ("server_harmony_test.go", "app/server/server_harmony_test.go"),
                       ("parent_lifeline_harmony.go", "app/cmd/harmony/parent_lifeline_harmony.go"),
                       ("wait_executable_harmony.go", "app/cmd/harmony/wait_executable_harmony.go"),
                       ("desired_elf_harmony.go", "app/cmd/harmony/desired_elf_harmony.go"),
                       ("desired_elf_harmony_test.go", "app/cmd/harmony/desired_elf_harmony_test.go"),
                       ("desired_native_harmony.go", "app/cmd/harmony/desired_native_harmony.go"),
                       ("wait_executable_native_harmony.go", "app/cmd/harmony/wait_executable_native_harmony.go"),
                       ("parent_lifeline_harmony_test.go", "app/cmd/harmony/parent_lifeline_harmony_test.go"),
                       ("core_output_default.go", "app/server/core_output_default.go"),
                       ("wait_group_default.go", "app/server/wait_group_default.go"),
                       ("wait_group_harmony.go", "app/server/wait_group_harmony.go"),
                       ("wait_group_harmony_test.go", "app/server/wait_group_harmony_test.go"),
                       ("core_output_harmony.go", "app/server/core_output_harmony.go"),
                       ("core_output_harmony_test.go", "app/server/core_output_harmony_test.go"),
                       ("updater_harmony.go", "app/updater/updater_harmony.go"),
                       ("updater_harmony_test.go", "app/updater/updater_harmony_test.go"),
                       ("onboarding_config_harmony_test.go", "cmd/config/onboarding_harmony_test.go"),
                       ("onboarding_harmony.go", "internal/onboarding/app_path_harmony.go"),
                       ("onboarding_default.go", "internal/onboarding/app_path_default.go")):
        target = source/dest
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ADAPTER/"app"/name, target)
    # Keep historical patch order reproducible, then apply the reviewed
    # non-HarmonyOS behavior correction. Reverse-check first for idempotence.
    isolation_patch = ADAPTER/"ui-platform-isolation.patch"
    if subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--reverse", "--check", str(isolation_patch)], cwd=source, capture_output=True).returncode != 0:
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", "--check", str(isolation_patch)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--unidiff-zero", "--recount", str(isolation_patch)], cwd=source, check=True)
    fixture_patch_sha = prepare_portable_tests(source)
    args.evidence.mkdir(parents=True, exist_ok=True)
    diff = subprocess.check_output(["git", "diff", "--binary"], cwd=source)
    (args.evidence/"upstream-tracked.patch").write_bytes(diff)
    (args.evidence/"source-adaptation.json").write_text(json.dumps({
        "commit": PIN, "downstream_commit": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip(), "build_tag_files": sorted(changed),
        **think_evidence,
        "test_fixture_patch_sha256": fixture_patch_sha,
        "ui_platform_isolation_patch_sha256": hashlib.sha256(isolation_patch.read_bytes()).hexdigest(),
        "tracked_diff_sha256": hashlib.sha256(diff).hexdigest(),
        "onboarding_patch_sha256": hashlib.sha256(patch.read_bytes()).hexdigest(),
        "ui_platform_patch_sha256": hashlib.sha256(ui_patch.read_bytes()).hexdigest(),
        "model_navigation_patch_sha256": hashlib.sha256(navigation_patch.read_bytes()).hexdigest(),
        "model_page_sha256": hashlib.sha256((ADAPTER/"ui/models.tsx").read_bytes()).hexdigest(),
        "model_page_test_sha256": hashlib.sha256((ADAPTER/"ui/models.test.tsx").read_bytes()).hexdigest(),
        "model_download_files": {name: hashlib.sha256((ADAPTER/"ui"/name).read_bytes()).hexdigest()
                                 for name in ("model-download.ts", "model-download.test.ts", "window-layout.ts", "window-layout.test.tsx")},
        "window_layout_patch_sha256": hashlib.sha256(window_patch.read_bytes()).hexdigest(),
        "capability_patch_sha256": hashlib.sha256(capability_patch.read_bytes()).hexdigest(),
        "login_startup_patch_sha256": hashlib.sha256(startup_patch.read_bytes()).hexdigest(),
        "login_startup_status_patch_sha256": hashlib.sha256(startup_status_patch.read_bytes()).hexdigest(),
        "login_startup_deduplicate_patch_sha256": hashlib.sha256(deduplicate_patch.read_bytes()).hexdigest(),
        "services_patch_sha256": hashlib.sha256(service_patch.read_bytes()).hexdigest(),
        "core_output_patch_sha256": hashlib.sha256(core_output_patch.read_bytes()).hexdigest(),
        "core_wait_patch_sha256": hashlib.sha256(core_wait_patch.read_bytes()).hexdigest(),
        "core_model_root_patch_sha256": hashlib.sha256(model_root_patch.read_bytes()).hexdigest(),
        "core_model_root_tests_patch_sha256": hashlib.sha256(root_tests.read_bytes()).hexdigest(),
        "storage_ui_patch_sha256": hashlib.sha256(storage_patch.read_bytes()).hexdigest(),
        "all_ui_adapters": {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                            for p in (ADAPTER/"ui").glob("*.ts*")},
        "services_ui_files": {name: hashlib.sha256((ADAPTER/"ui"/name).read_bytes()).hexdigest() for name in ("services.tsx", "services.test.tsx")},
        "login_startup_status_files": {name: hashlib.sha256((ADAPTER/"ui"/name).read_bytes()).hexdigest()
                                       for name in ("login-startup.tsx", "login-startup.test.tsx")},
        "adapters": {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                     for p in (ADAPTER/"app").glob("*.go")},
        "status": "prepared; not device-qualified"
    }, indent=2)+"\n")
    print(f"Prepared {len(changed)} common Go files and scoped UI platform adaptation")

if __name__ == "__main__":
    main()
