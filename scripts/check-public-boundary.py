#!/usr/bin/env python3
"""Fail closed on paths outside the reviewed public-repository allowlist.

This is a scope guard, not a substitute for content review or secret scanning.
Only Git-tracked files are checked so local caches/output do not enter releases.
"""
from pathlib import PurePosixPath
import subprocess
import sys

ROOT_FILES = {
    '.gitignore', 'AGENTS.md', 'README.md', 'LICENSE', 'NOTICE',
    'THIRD_PARTY_NOTICES.md', 'CONTRIBUTING.md', 'SECURITY.md', 'wing.config.yaml',
}
PREFIXES = (
    'server/', 'sdk/', 'docs/', 'third_party/licenses/',
)
EXACT_FILES = {
    '.github/workflows/ci.yml', '.github/workflows/cli.yml',
    'deploy/README.md', 'deploy/compose.yaml', 'deploy/game-stop.service',
    'deploy/nginx.conf.example', 'deploy/receive.py', 'deploy/test_receive.py',
    'scripts/check-public-boundary.py', 'scripts/package-cli.sh', 'tests/platform_sdk_test.gd',
}
FORBIDDEN_SUFFIXES = {
    '.ets', '.hap', '.har', '.hsp', '.apk', '.aab', '.ipa', '.pck', '.game',
    '.so', '.dll', '.dylib', '.exe', '.wasm', '.p12', '.pfx', '.jks',
    '.keystore', '.key', '.pem', '.db', '.sqlite', '.sqlite3',
}
FORBIDDEN_PARTS = {
    'app', 'frame', 'android', 'ios', 'harmony', 'node_modules', '.godot',
    'vendor', 'data', 'artifacts', '__pycache__',
}

def violations(paths):
    errors = []
    for path in paths:
        p = PurePosixPath(path)
        allowed = path in ROOT_FILES or path in EXACT_FILES or path.startswith(PREFIXES)
        forbidden = (p.suffix.lower() in FORBIDDEN_SUFFIXES
                     or bool(set(p.parts) & FORBIDDEN_PARTS)
                     or p.name.startswith('.env')
                     or p.name in {'known_hosts', 'server.env', 'id_rsa', 'id_ed25519'})
        if not allowed or forbidden:
            errors.append(path)
    return errors

# Keep the exclusion behavior exercised even when the current tree is clean.
assert all(violations([path]) for path in [
    'app/client.ets', 'frame/main.gd', 'server/.env',
    'server/data/users.sqlite', 'sdk/game.pck', '.github/workflows/deploy.yml',
])
assert not violations(['server/go.mod', 'sdk/godot/game_stop.gd',
                       'third_party/licenses/example/LICENSE'])

if __name__ == '__main__':
    raw = subprocess.check_output(['git', 'ls-files', '-z'])
    paths = [p for p in raw.decode('utf-8').split('\0') if p]
    if not paths:
        sys.exit('No tracked files: stage the reviewed source before checking.')
    errors = violations(paths)
    # Deleted client files are still public if their commits become reachable.
    # Inspect every fetched branch/tag, not only the current worktree.
    history = subprocess.check_output(['git', 'log', '--all', '--format=', '--name-only', '-z'])
    old_paths = {p.strip('\n') for p in history.decode('utf-8').split('\0') if p.strip('\n')}
    errors += ['history: ' + p for p in violations(sorted(old_paths))]
    if errors:
        print('Public boundary violation:\n' + '\n'.join(errors), file=sys.stderr)
        sys.exit(1)
    print(f'Public boundary OK: {len(paths)} tracked files and {len(old_paths)} historical paths; no native-client/binary/secret paths.')
