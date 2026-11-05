export const start: (filesDir: string, modelsDir: string, port: number) => string;
export const stop: (filesDir: string) => string;
export const status: (filesDir: string) => string;
export const logs: (filesDir: string) => string;
// Debug-build HDC validation only; release returns an error without reading argv.
export const command: (filesDir: string, args: string[], port: number) => string;
export const appStart: (filesDir: string, corePort: number, systemCaDirectory?: string) => string;
export const appStop: (filesDir: string) => string;
export const appToken: (filesDir: string) => string;
export const appShutdownStatus: (filesDir: string) => string;
// Debug-only, current direct APP instance; no arbitrary PID argument.
export const appDebugCrash: (filesDir: string) => string;
// Diagnostic only; ArkTS passes the URI from its own system picker record.
export const storagePermissionAudit: (uri: string) => string;
