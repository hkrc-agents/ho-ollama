"""Internal HDC test helper, not a user CLI or replacement terminal."""
import os
import json
from pathlib import Path
import re
import subprocess
import time

class HarmonyUI:
    def __init__(self, evidence, target='127.0.0.1:5555', server=None):
        self.evidence = Path(evidence)
        self.evidence.mkdir(parents=True, exist_ok=True)
        self.target = target
        if server is not None and (not re.fullmatch(r'(?:127\.0\.0\.1|::ffff:127\.0\.0\.1):[0-9]{1,5}',server)
                                   or not 1024<int(server.rsplit(':',1)[1])<=65535):
            raise ValueError('Explicit HDC test server must use a nonprivileged loopback port')
        self.server = server
        self.nodes = []
        self.tray_anchor = None

    def hdc(self, *args):
        command=[os.environ.get('HDC', 'hdc')]
        if self.server is not None:command+=['-s',self.server]
        result = subprocess.run([*command,'-t', self.target, *args], capture_output=True, timeout=60)
        private = 'app-token' in ' '.join(args)
        raw_files = []
        for channel in ('stdout', 'stderr'):
            value = getattr(result, channel)
            if isinstance(value, bytes):
                try:
                    decoded = value.decode('utf-8')
                except UnicodeDecodeError:
                    # System logs may contain malformed/truncated UTF-8. Keep
                    # original bytes without losing the diagnostic/cleanup path.
                    if not private:
                        path = self.evidence / ('hdc-' + str(time.time_ns()) + '-' + channel + '.bin')
                        path.write_bytes(value)
                        raw_files.append(path.name)
                    decoded = value.decode('utf-8', errors='replace')
                setattr(result, channel, decoded)
        with (self.evidence/'commands.log').open('a') as log:
            output = '[private token redacted]' if private else result.stdout+result.stderr
            log.write(json.dumps({'args':args,'exit':result.returncode,'output':output,
                                 'raw_invalid_utf8':raw_files},ensure_ascii=False)+'\n')
        transport_failure=(result.stdout+result.stderr).strip() in {'Connect server failed','[Fail]Connect server failed'}
        if result.returncode or '[Fail]' in result.stdout or transport_failure:
            raise RuntimeError(result.stdout+result.stderr)
        return result.stdout

    def shell(self, text): return self.hdc('shell', text)
    def action(self, name):
        return self.shell(f'aa start -b com.pico.ollama -a EntryAbility --ps action {name}')
    def snapshot(self, name, screenshot=True):
        remote = '/data/local/tmp/pico-migration-'+name
        command = f'uitest dumpLayout -p {remote}.json'
        if screenshot: command += f'; uitest screenCap -p {remote}.png'
        receipt=self.shell(command)
        if 'DumpLayout saved to:'+remote+'.json' not in receipt:
            raise RuntimeError('No actual layout collection receipt: '+receipt)
        if screenshot and 'ScreenCap saved to '+remote+'.png' not in receipt:
            raise RuntimeError('No actual screenshot collection receipt: '+receipt)
        for suffix in (('.json','.png') if screenshot else ('.json',)):
            reply=self.hdc('file','recv',remote+suffix,str(self.evidence/(name+suffix)))
            if 'FileTransfer finish' not in reply or not (self.evidence/(name+suffix)).is_file():
                raise RuntimeError('No actual UI artifact transfer: '+reply)
        nodes = []
        def visit(value):
            if isinstance(value, dict):
                if 'attributes' in value: nodes.append(value['attributes'])
                for child in value.values(): visit(child)
            elif isinstance(value,list):
                for child in value: visit(child)
        visit(json.loads((self.evidence/(name+'.json')).read_text()))
        self.nodes = nodes
        return nodes
    def find(self, label):
        return next((n for n in self.nodes if self.matches(n, label)
                     and n.get('visible') == 'true'), None)
    @staticmethod
    def matches(node, label):
        # ArkWeb exposes an empty input's aria-label plus placeholder in hint,
        # rather than text/description. Keep semantic selection, not coordinates.
        return (node.get('text') == label or node.get('description') == label or
                node.get('hint') == label or node.get('hint', '').startswith(label+' '))
    def click(self, label):
        matches = [n for n in self.nodes if self.matches(n, label)
                   and n.get('visible') == 'true']
        matches.sort(key=lambda n: (n.get('type','').lower() != 'button', n.get('clickable') != 'true'))
        node = matches[0] if matches else None
        if node is None: raise AssertionError('Missing UI control: '+label)
        if node.get('enabled') == 'false': raise AssertionError('Disabled UI control: '+label)
        x1,y1,x2,y2 = map(int,re.findall(r'-?\d+',node['bounds']))
        self.shell(f'uitest uiInput click {(x1+x2)//2} {(y1+y2)//2}')
    def wait_for(self, labels, name):
        for attempt in range(6):
            self.snapshot(name+'-'+str(attempt))
            if any(self.find(label) for label in labels): return
            time.sleep(1)
        raise AssertionError('UI did not show: '+str(labels))


def invoke_ollama_tray_action(ui, text, label, attempts=3):
    """Require a new HAP callback receipt, not just successful click injection.

    Retry only when dispatch was not acknowledged; never repeats an already
    received Quit or substitutes an aa/debug action for an actual menu click.
    """
    codes = {'Open Ollama': 'show', 'Settings': 'settings', 'View logs': 'logs', 'Quit Ollama': 'quit'}
    code = codes[text]
    def receipt():
        raw = ui.hdc('shell', '-b', 'com.pico.ollama', 'cat',
                     'data/storage/el2/base/haps/entry/files/tray-action.json').strip()
        if 'No such file' in raw:
            return None
        value = json.loads(raw)
        if not isinstance(value.get('received'), int) or not isinstance(value.get('code'), str):
            raise AssertionError('Malformed tray action receipt')
        return value
    before = receipt()
    for attempt in range(attempts):
        open_ollama_tray_menu(ui, label+'-open-'+str(attempt))
        # Measure actual controls, then reopen/select in one device-side input
        # sequence. Host round trips can outlive a transient desktop popup.
        target = ui.find(text)
        if target is None:
            continue
        if not ui.tray_anchor:
            raise AssertionError('No measured tray anchor')
        selection = dict(ui.tray_anchor, target_bounds=target['bounds'])
        ui.shell('uinput -K -d 2070 -u 2070') # SDK KEYCODE_ESCAPE; close the measured popup.
        open_ollama_tray_menu(ui, label+'-dispatch-'+str(attempt), selection=selection)
        for _ in range(3):
            value = receipt()
            if value and value['code'] == code and (before is None or value['received'] != before['received']):
                return value
            time.sleep(.2)
    raise AssertionError('No new tray action receipt after bounded actual-control clicks: '+text)


def open_ollama_tray_menu(ui, label, attempts=3, selection=None):
    """Bounded actual-control retry; never substitutes a debug Want for a click.

    Opening the system overflow and opening the application menu are distinct
    transitions. A single successful click injection does not prove either UI
    has appeared. Keep the final timeout a failure rather than clicking stale
    coordinates or treating an open main window as an open tray menu. A
    measured selection is accepted only with identical display/root/icon
    geometry, then open+select is injected without an intervening layout dump.
    """
    def node(key):
        return next((n for n in ui.nodes if n.get('id') == key and n.get('visible') == 'true'), None)

    def click(value):
        x1, y1, x2, y2 = map(int, re.findall(r'-?\d+', value['bounds']))
        output = ui.shell(f'uinput -M -m {(x1+x2)//2} {(y1+y2)//2} -c 0')
        if 'move to' not in output or 'Coordinate value' in output:
            raise AssertionError('Tray mouse input failed')

    def ready():
        return bool(ui.find('Open Ollama') and ui.find('Quit Ollama'))

    for attempt in range(attempts):
        ui.snapshot(label + '-before-' + str(attempt), screenshot=False)
        if ready():
            if selection is None and ui.tray_anchor:
                return
            ui.shell('uinput -K -d 2070 -u 2070')
            ui.snapshot(label+'-closed-'+str(attempt), screenshot=False)
        item = node('GridItemOllama for HarmonyOS')
        if item is None:
            arrow = node('DockStatusBarComponent_arrowbutton')
            if arrow is None:
                continue
            click(arrow)
            for poll in range(4):
                ui.snapshot(f'{label}-overflow-{attempt}-{poll}', screenshot=False)
                item = node('GridItemOllama for HarmonyOS')
                if item is not None:
                    break
                time.sleep(.25)
        if item is None:
            continue
        anchor = {'bounds': item['bounds'], 'display': item.get('displayId'),
                  'root_bounds': ui.nodes[0].get('bounds') if ui.nodes else None}
        if selection is not None:
            if any(selection.get(k) != value for k, value in anchor.items()):
                raise AssertionError('Tray geometry changed after measurement')
            x1,y1,x2,y2 = map(int,re.findall(r'-?\d+', item['bounds']))
            a1,b1,a2,b2 = map(int,re.findall(r'-?\d+', selection['target_bounds']))
            output=ui.shell(f'uinput -M -m {(x1+x2)//2} {(y1+y2)//2} -c 0 -i 250 -m {(a1+a2)//2} {(b1+b2)//2} -c 0')
            if 'move to' not in output or 'Coordinate value' in output:
                raise AssertionError('Tray mouse input failed')
            return
        ui.tray_anchor = anchor
        click(item)
        for poll in range(6):
            ui.snapshot(f'{label}-menu-{attempt}-{poll}', screenshot=False)
            if ready():
                return
            time.sleep(.25)
    ui.snapshot(label + '-failed')
    raise AssertionError('Ollama tray menu did not appear after bounded actual-control retries')
