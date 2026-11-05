"""Public-facing HAP packaging must not depend on a diagnostic ZIP extractor."""
import io
from pathlib import Path
import runpy
import stat
import tempfile
import unittest
import warnings
import zipfile

EXTRACT = runpy.run_path(str(Path(__file__).resolve().parents[1] / 'tools/hap_contract.py'))['extract_payload']


def archive(entries):
    data = io.BytesIO()
    with zipfile.ZipFile(data, 'w') as z:
        for name, mode in entries:
            info = zipfile.ZipInfo(name); info.external_attr = mode << 16
            z.writestr(info, b'fixture')
    data.seek(0)
    return zipfile.ZipFile(data)


class ArchiveTests(unittest.TestCase):
    def test_execute_mode_preserved_in_new_owned_directory(self):
        with tempfile.TemporaryDirectory() as folder, archive([('payload/bin/program', stat.S_IFREG | 0o755)]) as z:
            target = Path(folder) / 'unpack'; EXTRACT(z, target)
            self.assertEqual((target / 'payload/bin/program').read_bytes(), b'fixture')
            self.assertEqual(stat.S_IMODE((target / 'payload/bin/program').stat().st_mode), 0o755)

    def test_unsafe_paths_rejected_before_creating_destination(self):
        for name in ('../outside', '/outside', 'a/../../outside', 'a\\outside', 'a//b'):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as folder, archive([(name, stat.S_IFREG | 0o644)]) as z:
                target = Path(folder) / 'unpack'
                with self.assertRaisesRegex(ValueError, 'Unsafe archive path'):
                    EXTRACT(z, target)
                self.assertFalse(target.exists())

    def test_symlinks_and_privileged_modes_rejected(self):
        for mode in (stat.S_IFLNK | 0o777, stat.S_IFREG | 0o4755, stat.S_IFCHR | 0o600):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as folder, archive([('payload/file', mode)]) as z:
                target = Path(folder) / 'unpack'
                with self.assertRaisesRegex(ValueError, 'Unsupported archive'):
                    EXTRACT(z, target)
                self.assertFalse(target.exists())

    def test_existing_destination_not_overwritten(self):
        with tempfile.TemporaryDirectory() as folder, archive([('file', stat.S_IFREG | 0o644)]) as z:
            target = Path(folder); (target / 'file').write_text('original')
            with self.assertRaisesRegex(ValueError, 'new directory'):
                EXTRACT(z, target)
            self.assertEqual((target / 'file').read_text(), 'original')

    def test_duplicate_names_rejected_before_extraction(self):
        with tempfile.TemporaryDirectory() as folder, warnings.catch_warnings():
            warnings.simplefilter('ignore', UserWarning)
            with archive([('file', stat.S_IFREG | 0o644), ('file', stat.S_IFREG | 0o644)]) as z:
                target = Path(folder) / 'unpack'
                with self.assertRaisesRegex(ValueError, 'Duplicate'):
                    EXTRACT(z, target)
                self.assertFalse(target.exists())


if __name__ == '__main__':
    unittest.main()
