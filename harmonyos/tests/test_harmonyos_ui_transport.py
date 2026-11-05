import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('harmony_ui',Path(__file__).resolve().parents[1]/'tools/harmonyos_ui.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)


class UiTransport(unittest.TestCase):
    def test_empty_target_is_rejected(self):
        with tempfile.TemporaryDirectory() as folder:
            with self.assertRaises(ValueError):
                module.HarmonyUI(folder,target='')

    def test_invalid_diagnostic_utf8_retains_original_bytes(self):
        with tempfile.TemporaryDirectory() as folder:
            ui=module.HarmonyUI(folder,target='physical')
            reply=subprocess.CompletedProcess([],0,b'prefix\xe6tail',b'')
            with patch.object(module.subprocess,'run',return_value=reply):
                self.assertEqual(ui.hdc('shell','hilog -x'),'prefix\ufffdtail')
            raw=list(Path(folder).glob('*.bin'))
            self.assertEqual(len(raw),1)
            self.assertEqual(raw[0].read_bytes(),b'prefix\xe6tail')

    def test_private_token_invalid_bytes_are_never_written(self):
        with tempfile.TemporaryDirectory() as folder:
            ui=module.HarmonyUI(folder,target='physical')
            reply=subprocess.CompletedProcess([],0,b'secret\xff',b'')
            with patch.object(module.subprocess,'run',return_value=reply):
                ui.hdc('shell','cat app-token')
            self.assertFalse(list(Path(folder).glob('*.bin')))
            self.assertNotIn('secret',(Path(folder)/'commands.log').read_text())

    def test_explicit_owned_server_is_scoped_and_default_unchanged(self):
        with tempfile.TemporaryDirectory() as folder:
            reply=subprocess.CompletedProcess([],0,'ok','')
            for server in (None,'127.0.0.1:18721'):
                ui=module.HarmonyUI(folder,target='physical',server=server)
                with patch.object(module.subprocess,'run',return_value=reply) as call:
                    ui.hdc('shell','id')
                    args=call.call_args.args[0]
                    self.assertEqual('-s' in args,server is not None)
                    self.assertIn('physical',args)
            for invalid in ['remote.example:18721','127.0.0.1:80','127.0.0.1:99999']:
                with self.assertRaises(ValueError):module.HarmonyUI(folder,target="physical",server=invalid)

    def test_server_failure_with_exit_zero_is_not_success(self):
        with tempfile.TemporaryDirectory() as folder:
            ui=module.HarmonyUI(folder,target='physical')
            result=subprocess.CompletedProcess([],0,'Connect server failed\n','')
            with patch.object(module.subprocess,'run',return_value=result):
                with self.assertRaises(RuntimeError):ui.hdc('file','recv','remote','local')

    def test_layout_failure_cannot_read_an_old_file(self):
        with tempfile.TemporaryDirectory() as folder:
            ui=module.HarmonyUI(folder,target='physical');(Path(folder)/'same.json').write_text('{}')
            with patch.object(ui,'shell',return_value='Connect server failed\n'),patch.object(ui,'hdc') as transfer:
                with self.assertRaises(RuntimeError):ui.snapshot('same',screenshot=False)
                transfer.assert_not_called()

    def test_empty_transfer_receipt_is_rejected(self):
        with tempfile.TemporaryDirectory() as folder:
            ui=module.HarmonyUI(folder,target='physical')
            with patch.object(ui,'shell',return_value='DumpLayout saved to:/data/local/tmp/pico-migration-test.json\n'),patch.object(ui,'hdc',return_value=''):
                with self.assertRaises(RuntimeError):ui.snapshot('test',screenshot=False)

    def test_exact_collection_and_transfer_receipts(self):
        with tempfile.TemporaryDirectory() as folder:
            ui=module.HarmonyUI(folder,target='physical')
            def transfer(*args):
                Path(args[-1]).write_text('{"attributes":{"text":"fresh"}}')
                return 'FileTransfer finish, Size:1, File count = 1'
            with patch.object(ui,'shell',return_value='DumpLayout saved to:/data/local/tmp/pico-migration-test.json\n'),patch.object(ui,'hdc',side_effect=transfer):
                self.assertEqual(ui.snapshot('test',screenshot=False)[0]['text'],'fresh')


if __name__=='__main__':unittest.main()
