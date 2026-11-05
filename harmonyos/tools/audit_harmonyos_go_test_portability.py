"""Require preserved upstream test identities, no skips and explicit mock scope."""
import argparse
import collections
import hashlib
import json
from pathlib import Path
import subprocess

NEW_TESTS = {'github.com/ollama/ollama/app/updater/TestHarmonyDoesNotOfferDesktopInstaller',
             'github.com/ollama/ollama/cmd/config/TestHarmonyWelcomePreservesConfigAndUsesMarker'}
CHANGED_TESTS = {'app/ui/ui_test.go', 'app/updater/updater_test.go',
                 'cmd/config/config_test.go', 'app/updater/updater_harmony_test.go',
                 'cmd/config/onboarding_harmony_test.go'}
EXPECTED_FAILURES = {'github.com/ollama/ollama/app/ui/TestGetIntegrationStatuses',
                     'github.com/ollama/ollama/app/updater/TestBackgoundChecker',
                     'github.com/ollama/ollama/app/updater/TestAutoUpdateReenabledDownloadsUpdate',
                     'github.com/ollama/ollama/app/updater/TestCancelOngoingDownload',
                     'github.com/ollama/ollama/cmd/config/TestWelcomeStorage'}


def ledger(path):
    actions, names, packages = collections.Counter(), collections.defaultdict(collections.Counter), {}
    guards = []
    for line in path.read_text().splitlines():
        try:
            row = json.loads(line)
        except ValueError:
            continue
        if row.get('host_test_guard'):
            guards.append(row)
        if row.get('Action') in ('pass', 'fail', 'skip'):
            if row.get('Test'):
                actions[row['Action']] += 1
                names[row['Package'] + '/' + row['Test']][row['Action']] += 1
            else:
                packages[row['Package']] = row['Action']
    if len(guards) != 1 or int(guards[0]['effective_capabilities'], 16) != 0:
        raise ValueError('Missing or invalid actual isolation receipt')
    return {'actions': actions, 'names': dict(names), 'packages': packages}


def required_run(result, expected, repetitions, packages):
    if set(result['names']) != expected:
        raise ValueError('Missing or unexpected original test identities')
    if result['actions'].get('fail') or result['actions'].get('skip'):
        raise ValueError('Failed/skipped tests are not a passing lane')
    if any(counts != {'pass': repetitions} for counts in result['names'].values()):
        raise ValueError('Unexpected per-test execution multiplicity')
    if result['packages'] != {name: 'pass' for name in packages}:
        raise ValueError('Package result missing or failed')


def audit(root, replica):
    manifest = json.loads((root / 'result.json').read_text())
    if (not manifest['pass'] or not manifest['original_source_unchanged'] or
            manifest['production_source_changed'] or not manifest['preparation_idempotent'] or
            set(manifest['changed_tests']) != CHANGED_TESTS):
        raise ValueError('Source isolation or test-only scope was not proven')
    rows = {row['name']: row for row in manifest['runs']}
    expected_runs = {'before', 'after-race', 'repeat', 'linux-default-control'}
    if set(rows) != expected_runs or len(manifest['runs']) != 4:
        raise ValueError('Missing or duplicate required test lanes')
    ledgers = {}
    for name, row in rows.items():
        path = root / (name + '.jsonl')
        if hashlib.sha256(path.read_bytes()).hexdigest() != row['log_sha256'] or not row['guard_verified']:
            raise ValueError('Evidence hash or isolation mismatch')
        ledgers[name] = ledger(path)
    before = ledgers['before']
    if rows['before']['exit'] == 0 or before['actions']['fail'] != 5 or before['actions']['skip']:
        raise ValueError('The five original failures were not reproduced')
    if {name for name, actions in before['names'].items() if actions.get('fail')} != EXPECTED_FAILURES:
        raise ValueError('The baseline failures changed identity')
    expected = set(before['names']) | NEW_TESTS
    packages = set(before['packages'])
    for name, repetitions in [('after-race', 1), ('repeat', 3)]:
        if rows[name]['exit'] != 0:
            raise ValueError('Failed rerun: ' + name)
        required_run(ledgers[name], expected, repetitions, packages)
    config_prefix = 'github.com/ollama/ollama/cmd/config'
    expected_config = {name for name in before['names'] if name.startswith(config_prefix + '/')}
    if rows['linux-default-control']['exit'] != 0:
        raise ValueError('Default Linux control failed')
    required_run(ledgers['linux-default-control'], expected_config, 1, {config_prefix})
    assets = replica / 'app/ui/app/dist'
    actual = {str(path.relative_to(assets)): hashlib.sha256(path.read_bytes()).hexdigest()
              for path in assets.rglob('*') if path.is_file()}
    if actual != manifest['frontend_assets']:
        raise ValueError('Real frontend embed assets changed')
    expected_sources = dict(manifest['original_source'])
    expected_sources.update({name: record['after'] for name, record in manifest['changed_tests'].items()})
    names = subprocess.check_output(['git', 'ls-files', '-co', '--exclude-standard', '-z'], cwd=replica).split(b'\0')
    actual_names = {name.decode() for name in names if name}
    if actual_names != set(expected_sources):
        raise ValueError('Prepared source file set drifted')
    for name, expected_hash in expected_sources.items():
        path = replica / name
        if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != expected_hash:
            raise ValueError('Prepared source hash drifted: ' + name)
    return {'pass': True, 'original_test_identities': len(before['names']),
            'after_race': dict(ledgers['after-race']['actions']),
            'repeat_three': dict(ledgers['repeat']['actions']),
            'linux_default_control': dict(ledgers['linux-default-control']['actions']),
            'production_source_changed': False, 'deployed': False, 'release_qualified': False,
            'all_project_work_complete': False,
            'audit_script_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            'producer_manifest_sha256': hashlib.sha256((root / 'result.json').read_bytes()).hexdigest(),
            'scope': 'Test-fixture/platform expectation repair, not enabled HarmonyOS updater or unchanged-upstream qualification'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--replica', type=Path, required=True)
    args = parser.parse_args()
    try:
        result = audit(args.root, args.replica)
    except Exception as error:
        result = {'pass': False, 'release_qualified': False, 'all_project_work_complete': False, 'error': str(error)}
    with (args.root / 'audit.json').open('x') as stream:
        json.dump(result, stream, indent=2)
    print(json.dumps(result))
    raise SystemExit(0 if result['pass'] else 1)


if __name__ == '__main__':
    main()
