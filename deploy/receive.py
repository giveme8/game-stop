#!/usr/bin/python3
"""Forced SSH command: accept exactly two binaries, never run uploaded shell code."""
import fcntl
import gzip
import io
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path('/opt/game-stop')


class BoundedReader:
    """Limit decompressed tar bytes, including extension headers, not just files."""
    def __init__(self, source, limit=257 * 1024 * 1024):
        self.source = source
        self.remaining = limit

    def read(self, size=-1):
        size = self.remaining + 1 if size < 0 else min(size, self.remaining + 1)
        data = self.source.read(size)
        self.remaining -= len(data)
        if self.remaining < 0:
            raise ValueError('Decompressed archive exceeds total byte limit')
        return data


def activate(target):
    temporary = ROOT / 'current.next'
    temporary.unlink(missing_ok=True)
    temporary.symlink_to(target)
    temporary.replace(ROOT / 'current')


def restart():
    subprocess.run(['sudo', '-n', '/usr/bin/systemctl', 'restart', 'game-stop.service'], check=True)


def main():
    command = os.environ.get('SSH_ORIGINAL_COMMAND', '')
    if not re.fullmatch(r'deploy [0-9a-f]{40}', command):
        raise ValueError('Only deploy COMMIT_SHA is accepted')
    sha = command.split()[1]
    with (ROOT / 'deploy.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        release = ROOT / 'releases' / sha
        if release.exists():
            raise ValueError('Release already exists; use a new commit or operator-reviewed cleanup')
        staging = Path(tempfile.mkdtemp(prefix='.incoming-', dir=ROOT / 'releases'))
        try:
            found = set()
            buffered = io.BufferedReader(sys.stdin.buffer)
            source = gzip.GzipFile(fileobj=buffered) if buffered.peek(2)[:2] == b'\x1f\x8b' else buffered
            with tarfile.open(fileobj=BoundedReader(source), mode='r|') as archive:
                for member in archive:
                    if member.name not in {'server', 'gamestop'} or member.name in found or not member.isfile():
                        raise ValueError('Archive must contain only server and gamestop regular files')
                    if not 0 < member.size <= 128 * 1024 * 1024:
                        raise ValueError('Invalid binary size')
                    found.add(member.name)
                    with archive.extractfile(member) as source, (staging / member.name).open('xb') as out:
                        shutil.copyfileobj(source, out)
                    if (staging / member.name).read_bytes()[:4] != b'\x7fELF':
                        raise ValueError('Not an ELF executable')
                    (staging / member.name).chmod(0o755)
            if found != {'server', 'gamestop'}:
                raise ValueError('Missing binaries')
            staging.chmod(0o755)
            staging.rename(release)
        finally:
            if staging.exists():
                shutil.rmtree(staging)
        current = ROOT / 'current'
        previous = current.resolve() if current.is_symlink() else None
        activate(release)
        try:
            restart()
            subprocess.run(['curl', '--fail', '--silent', '--show-error', '--retry', '15',
                            '--retry-all-errors', '--retry-delay', '2', '--max-time', '3',
                            '--retry-max-time', '60', 'http://127.0.0.1:18080/healthz'], check=True)
        except BaseException:
            if previous is not None:
                activate(previous)
                restart()
                print('Rolled back binaries; database and data untouched', file=sys.stderr)
            else:
                current.unlink(missing_ok=True)
                print('First release failed; no previous binary exists', file=sys.stderr)
            raise
        print('\nDeployed ' + sha)


if __name__ == '__main__':
    main()
