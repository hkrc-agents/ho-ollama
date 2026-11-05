"""Execute the unchanged-platform wait hook without HarmonyOS build tags.

This is a Linux contract, not Windows/macOS product CI qualification.
"""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
GO = os.environ.get('PICO_GO', 'go')
HARNESS = r'''
package server
import (
    "errors"
    "os/exec"
    "testing"
)
func TestDefaultWaitPreservesExit(t *testing.T) {
    for _, code := range []string{"0", "7"} {
        cmd := exec.Command("/bin/sh", "-c", "exit "+code)
        if err := cmd.Start(); err != nil { t.Fatal(err) }
        err := platformCoreWait(cmd)
        if code == "0" && err != nil { t.Fatal(err) }
        if code == "7" {
            var failure *exec.ExitError
            if !errors.As(err, &failure) || failure.ExitCode() != 7 { t.Fatalf("lost exit status: %v", err) }
        }
        if !platformCoreMayRestart(err) { t.Fatal("default restart policy changed") }
    }
}
'''


class DefaultWait(unittest.TestCase):
    def test_linux_default_wait_is_plain_command_wait(self):
        with tempfile.TemporaryDirectory(prefix='pico-default-wait-') as folder:
            root = Path(folder)
            (root/'go.mod').write_text('module pico-default-wait\n\ngo 1.26\n')
            source = ROOT/'app/wait_group_default.go'
            (root/'wait.go').write_bytes(source.read_bytes())
            (root/'wait_test.go').write_text(HARNESS)
            cache = root/'cache'; cache.mkdir()
            env = dict(os.environ, GOCACHE=str(cache), GOTOOLCHAIN='local', GOPROXY='off', GOMAXPROCS='4')
            run = subprocess.run([str(GO), 'test', '-count=1', '-v', '.'], cwd=root,
                                 env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=90)
            self.assertEqual(run.returncode, 0, run.stdout.decode(errors='replace'))
            self.assertIn(b'--- PASS: TestDefaultWaitPreservesExit', run.stdout)


if __name__ == '__main__':
    unittest.main()
