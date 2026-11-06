# HarmonyOS service launch designs

The current CPU build uses executable processes. An earlier experiment used
HAP-managed native-child shared libraries. Both designs have to supervise APP,
core, inference runner and independent server lifetimes, with explicit
ownership and cleanup when an owning process exits.

| Concern | Executable route used by the maintained build | HAP-managed native-child experiment |
|---|---|---|
| Launch | The HAP launches the packaged Go application. The application supervises executable services. Ollama starts its matched inference runner. | The HAP native layer calls `OH_Ability_StartNativeChildProcess` for shared-library entries. A HAP registry tracks logical ownership across APP, core, runner and server. |
| Lifetime | The port uses executable wait semantics, parent lifelines and subreaper-based descendant cleanup. | Official child callbacks and a per-session registry track instances and generations. For registered identities, verified process disappearance was a fallback for missing callbacks. |
| Control | The regular executable and packaging tools build this path. | A bounded private control channel carried startup, cancellation and drain requests. This required separate SO entry points and experimental Go toolchain work. |
| Evidence | This is the default source and packaging path documented in [HARMONYOS.md](../HARMONYOS.md). The exact architecture, signing and device scope is tracked in [STATUS.md](STATUS.md). | Host tests and an emulator fixture exercised registry behavior and real native-child creation for bounded fake services. Full production Go APP/core, model inference and ARM64 release acceptance were outside that experiment. |

For the executable implementation, start with
[`parent_lifeline_harmony.go`](app/parent_lifeline_harmony.go),
[`wait_group_harmony.go`](app/wait_group_harmony.go), and
[`service_launcher.c`](service_launcher.c). Build and package from the commands
in [HARMONYOS.md](../HARMONYOS.md).

The earlier SO experiment is archived in the private integration snapshot under
`repos/ollama/harmonyos/native-services/` and
`exp/HARMONYOS_NATIVE_SERVICE_MANAGER_20261023.md`. An exit callback can arrive
late or go missing, a kill request needs independent confirmation, and a Go
runtime with live threads requires process-level lifetime management. These
constraints remain relevant if the SO design is revisited.
