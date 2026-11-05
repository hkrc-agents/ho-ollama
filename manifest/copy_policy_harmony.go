//go:build harmonyos

package manifest

// HarmonyOS application sandboxes may reject symlink creation. Keep the
// upstream verified-blob and atomic ordinary-file replacement path.
const manifestCopyOnly = true
