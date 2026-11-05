"""Reproduce and repair five desktop-only test assumptions in an isolated tree.

No device, deployment, production-source edits, external network or publication.
All Go tests run in a private loopback namespace with Landlock write confinement.
"""
import argparse
import collections
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
EXPECTED_FAILURES = {
    'github.com/ollama/ollama/app/ui/TestGetIntegrationStatuses',
    'github.com/ollama/ollama/app/updater/TestBackgoundChecker',
    'github.com/ollama/ollama/app/updater/TestAutoUpdateReenabledDownloadsUpdate',
    'github.com/ollama/ollama/app/updater/TestCancelOngoingDownload',
    'github.com/ollama/ollama/cmd/config/TestWelcomeStorage',
}
CHANGED_TESTS = {'app/ui/ui_test.go', 'app/updater/updater_test.go',
                 'cmd/config/config_test.go', 'app/updater/updater_harmony_test.go',
                 'cmd/config/onboarding_harmony_test.go'}


def git(source, *args):
    return subprocess.check_output(['git', *args], cwd=source)


def snapshot(source):
    names = git(source, 'ls-files', '-co', '--exclude-standard', '-z').split(b'\0')
    result = {}
    for raw in sorted(set(names)):
        if raw:
            name = Path(raw.decode())
            path = source / name
            if name.is_absolute() or '..' in name.parts or path.is_symlink():
                raise ValueError('Unexpected source path: ' + str(name))
            if path.is_file():
                result[str(name)] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result


def events(path):
    counts, top, failures, packages = collections.Counter(), collections.Counter(), set(), {}
    guarded = False
    for line in path.read_text().splitlines():
        try:
            item = json.loads(line)
        except ValueError:
            continue
        if item.get('host_test_guard'):
            guarded = item['effective_capabilities'] == '0000000000000000' and item['uid'] == os.getuid()
        if item.get('Action') in ('pass', 'fail', 'skip'):
            name = item.get('Test')
            if name:
                counts[item['Action']] += 1
                if '/' not in name:
                    top[item['Action']] += 1
                    if item['Action'] == 'fail':
                        failures.add(item['Package'] + '/' + name)
            else:
                packages[item['Package']] = item['Action']
    return {'guard_verified': guarded, 'events': dict(counts), 'top_level': dict(top),
            'failures': sorted(failures), 'packages': packages}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--replica', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--go', type=Path, required=True)
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=False)
    result = {'pass': False, 'deployed': False, 'release_qualified': False, 'all_project_work_complete': False, 'runs': []}
    before = None
    lock_path = Path.home() / '.cache/ho-ollama/runtime-build.lock'
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    with lock_path.open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            if args.replica.exists() or args.replica.is_symlink():
                raise ValueError('Use a fresh isolated replica')
            before = snapshot(args.source)
            pin = git(args.source, 'rev-parse', 'HEAD').decode().strip()
            if pin != '8a971df3bacec93944ec894f2939fd2567afc5dd':
                raise ValueError('Unexpected upstream source revision')
            with (args.out / 'replica.log').open('wb') as log:
                subprocess.run(['git', 'clone', '--shared', '--no-checkout', str(args.source.resolve()), str(args.replica.resolve())], stdout=log, stderr=subprocess.STDOUT, check=True)
                subprocess.run(['git', 'checkout', '--detach', pin], cwd=args.replica, stdout=log, stderr=subprocess.STDOUT, check=True)
                subprocess.run(['git', 'apply', '-'], cwd=args.replica, input=git(args.source, 'diff', '--binary'), stdout=log, stderr=subprocess.STDOUT, check=True)
            for raw in git(args.source, 'ls-files', '--others', '--exclude-standard', '-z').split(b'\0'):
                if not raw:
                    continue
                name = Path(raw.decode())
                if name.suffix not in {'.go', '.ts', '.tsx'}:
                    raise ValueError('Unexpected untracked input: ' + str(name))
                target = args.replica / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(args.source / name, target)
            if snapshot(args.replica) != before:
                raise ValueError('Replica does not match the original prepared source')
            dist = args.source / 'app/ui/app/dist'
            assets = {}
            for path in sorted(dist.rglob('*')):
                if path.is_symlink():
                    raise ValueError('Frontend asset symlink rejected')
                if path.is_file():
                    assets[str(path.relative_to(dist))] = hashlib.sha256(path.read_bytes()).hexdigest()
            if not assets or 'index.html' not in assets:
                raise ValueError('Real generated frontend required; no placeholder embed')
            shutil.copytree(dist, args.replica / 'app/ui/app/dist')
            result.update(commit=pin, frontend_assets=assets, original_source=before)
            environment = {key: value for key, value in os.environ.items()
                           if not key.startswith(('OLLAMA_', 'PICO_', 'VERIFY_', 'FETCH_', 'HF_', 'HUGGING_'))}
            environment.update(GOTOOLCHAIN='local', GOOS='linux', GOARCH='amd64', CGO_ENABLED='1',
                               CC='cc', CXX='c++', GOFLAGS='-mod=readonly', GOPROXY='off', GOSUMDB='off',
                               PYTHONDONTWRITEBYTECODE='1')
            cache = subprocess.check_output([str(args.go), 'env', 'GOCACHE'], env=environment, text=True).strip()
            environment['GOMODCACHE'] = subprocess.check_output([str(args.go), 'env', 'GOMODCACHE'], env=environment, text=True).strip()

            def run(name, packages, tags=True, race=False, count=1):
                with tempfile.TemporaryDirectory(prefix='pico-test-portability-') as scratch:
                    env = dict(environment, HOME=scratch, USERPROFILE=scratch, LOCALAPPDATA=scratch,
                               APPDATA=scratch, XDG_CONFIG_HOME=scratch, XDG_CACHE_HOME=scratch,
                               TMPDIR=scratch, GOTMPDIR=scratch, GOCACHE=cache,
                               PICO_TEST_PARENT_NETNS=str(os.stat('/proc/self/ns/net').st_ino),
                               OLLAMA_MODELS=str(Path(scratch) / 'models'))
                    command = ['unshare', '-U', '-n', '--map-current-user', '--keep-caps', sys.executable, '-B',
                               str(ROOT / 'tools/confined_exec.py'), scratch, cache, '/dev/null', '--',
                               str(args.go), 'test', '-json', '-p', '1', '-count=' + str(count), '-timeout=120s']
                    if tags:
                        command += ['-tags=harmonyos']
                    if race:
                        command += ['-race']
                    command += packages
                    log_path = args.out / (name + '.jsonl')
                    # Collect on ext4 before copying; DrvFS logging can distort timeouts.
                    with tempfile.TemporaryFile(dir=scratch) as log:
                        child = subprocess.Popen(command, cwd=args.replica, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
                        try:
                            code = child.wait(timeout=420)
                        except subprocess.TimeoutExpired:
                            os.killpg(child.pid, signal.SIGTERM)
                            try:
                                child.wait(timeout=5)
                            except subprocess.TimeoutExpired:
                                os.killpg(child.pid, signal.SIGKILL)
                                child.wait()
                            raise RuntimeError('Bounded test run timed out: ' + name)
                        finally:
                            log.seek(0)
                            with log_path.open('wb') as target:
                                shutil.copyfileobj(log, target)
                    row = {'name': name, 'command': command, 'exit': code, **events(log_path),
                           'log_sha256': hashlib.sha256(log_path.read_bytes()).hexdigest()}
                    result['runs'].append(row)
                    if not row['guard_verified']:
                        raise RuntimeError('Host isolation guard not confirmed: ' + name)
                    print(json.dumps({k: row[k] for k in ('name', 'exit', 'events', 'failures')}), flush=True)
                    return row

            packages = ['./app/ui', './app/updater', './cmd/config']
            baseline = run('before', packages)
            if baseline['exit'] == 0 or set(baseline['failures']) != EXPECTED_FAILURES:
                raise RuntimeError('Baseline did not reproduce exactly the five recorded failures')
            with (args.out / 'prepare.log').open('wb') as log:
                subprocess.run([sys.executable, str(ROOT / 'tools/prepare_official_app.py'),
                                '--source', str(args.replica.resolve()), '--evidence', str((args.out / 'prepared').resolve())],
                               stdout=log, stderr=subprocess.STDOUT, check=True, timeout=240)
            after = snapshot(args.replica)
            changed = {name for name in before.keys() | after.keys() if before.get(name) != after.get(name)}
            if changed != CHANGED_TESTS:
                raise ValueError('Unexpected preparation delta: ' + repr(changed ^ CHANGED_TESTS))
            result['changed_tests'] = {name: {'before': before.get(name), 'after': after.get(name)} for name in sorted(changed)}
            # The second replay must be byte-for-byte idempotent.
            with (args.out / 'prepare-replay.log').open('wb') as log:
                subprocess.run([sys.executable, str(ROOT / 'tools/prepare_official_app.py'),
                                '--source', str(args.replica.resolve()), '--evidence', str((args.out / 'prepared-replay').resolve())],
                               stdout=log, stderr=subprocess.STDOUT, check=True, timeout=240)
            if snapshot(args.replica) != after:
                raise ValueError('Preparation replay changed the tree')
            for row in [run('after-race', packages, race=True),
                        run('repeat', packages, count=3),
                        run('linux-default-control', ['./cmd/config'], tags=False, race=True)]:
                if row['exit'] != 0 or row['events'].get('fail') or row['events'].get('skip') or not row['events'].get('pass'):
                    raise RuntimeError('Required test run did not pass without skips: ' + row['name'])
            if snapshot(args.replica) != after:
                raise ValueError('Tests modified prepared source')
            result.update({'pass': True, 'production_source_changed': False, 'preparation_idempotent': True})
        except Exception as error:
            result['error'] = str(error)
        finally:
            if before is not None:
                result['original_source_unchanged'] = snapshot(args.source) == before
                if not result['original_source_unchanged']:
                    result.update({'pass': False, 'error': 'Original prepared source changed'})
    (args.out / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps({key: value for key, value in result.items() if key not in ('original_source', 'frontend_assets', 'runs')}))
    raise SystemExit(0 if result['pass'] else 1)


if __name__ == '__main__':
    main()
