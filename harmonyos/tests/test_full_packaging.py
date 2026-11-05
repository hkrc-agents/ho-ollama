"""Reject incomplete or changed fresh runtime payloads before invoking SDK tools."""
import hashlib
from pathlib import Path
import sys
import tempfile
import unittest
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'tools'))
from package_full_hap import verified_files

class FreshPayload(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.expected = {}
        for name in ['bin/ollama', 'bin/ollama-app', 'bin/pico-service-launcher', 'bin/pico-storage-probe', 'lib/ollama/llama-server', 'bin/llama-cli', 'bin/llama-bench', 'bin/llama-quantize', 'bin/llama-simple']:
            p = self.root / name
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(name.encode())
            self.expected[name] = hashlib.sha256(p.read_bytes()).hexdigest()

    def test_complete_payload(self):
        self.assertEqual(verified_files(self.root, self.expected), self.expected)

    def test_changed_bytes(self):
        (self.root / 'bin/ollama').write_bytes(b'changed')
        with self.assertRaises(ValueError):
            verified_files(self.root, self.expected)

    def test_extra_unrecorded_file(self):
        (self.root / 'unrecorded').write_bytes(b'extra')
        with self.assertRaises(ValueError):
            verified_files(self.root, self.expected)

    def test_incomplete_record_cannot_hide_missing_component(self):
        (self.root / 'bin/ollama').unlink()
        del self.expected['bin/ollama']
        with self.assertRaises(ValueError):
            verified_files(self.root, self.expected)

    def test_symlink_even_when_target_bytes_match(self):
        p = self.root / 'bin/ollama'
        p.unlink()
        p.symlink_to('ollama-app')
        with self.assertRaises(ValueError):
            verified_files(self.root, self.expected)

if __name__ == '__main__':
    unittest.main()
