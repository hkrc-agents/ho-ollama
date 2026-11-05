"""Run the ArkTS storage boundary tests with the installed web test toolchain."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess

ADAPTER = Path(__file__).resolve().parents[1]

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--web', type=Path, required=True, help='Prepared app/ui/app with npm dependencies installed')
    p.add_argument('--out', type=Path, required=True)
    a = p.parse_args()
    a.out = a.out.resolve()
    deps = a.web.resolve()/'node_modules'
    runner = deps/'vitest/vitest.mjs'
    if not runner.is_file():
        p.error('Install the web dependencies first')
    a.out.mkdir(parents=True, exist_ok=False)
    (a.out/'node_modules').symlink_to(deps, target_is_directory=True)
    shutil.copy2(ADAPTER/'ui/model-storage-service.test.ts', a.out/'storage.test.ts')
    config = a.out/'vitest.config.mjs'
    config.write_text('export default { test: { include: ["storage.test.ts"], environment: "node", pool: "forks", maxWorkers: 1 } };\n')
    env = dict(os.environ, PICO_MODEL_STORAGE_SOURCE=str(ADAPTER/'hap/entry/src/main/ets/services/ModelStorageService.ets'))
    subprocess.run(['node', str(runner), 'run', '--config', str(config), '--reporter=json', '--outputFile='+str(a.out/'results.json')],
                   cwd=a.out, env=env, check=True, timeout=120)
    results = json.loads((a.out/'results.json').read_text())
    if not results.get('success') or not results.get('numTotalTests') or results.get('numPendingTests'):
        raise RuntimeError('Nonempty unskipped storage boundary suite required')
    print('Passed', results['numPassedTests'], 'storage boundary tests')

if __name__ == '__main__':
    main()
