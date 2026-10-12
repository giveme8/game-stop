import io
import gzip
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import receive


def archive(entries, compressed=False):
    result = io.BytesIO()
    with tarfile.open(fileobj=result, mode='w:gz' if compressed else 'w') as tar:
        for name, payload in entries:
            info = tarfile.TarInfo(name)
            info.size = len(payload)
            tar.addfile(info, io.BytesIO(payload))
    result.seek(0)
    return result


class ReceiverTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        (self.root / 'releases').mkdir()
        self.addCleanup(self.temp.cleanup)

    def run_receiver(self, entries, failure=False, command='deploy ' + 'a' * 40, compressed=False):
        stdin = type('Input', (), {'buffer': archive(entries, compressed)})()
        def run(args, **kwargs):
            if failure and args[0] == 'curl':
                raise subprocess.CalledProcessError(22, args)
        with patch.object(receive, 'ROOT', self.root), patch.object(receive.sys, 'stdin', stdin), \
             patch.dict(os.environ, SSH_ORIGINAL_COMMAND=command), patch.object(receive.subprocess, 'run', side_effect=run):
            receive.main()

    def test_success(self):
        self.run_receiver([('server', b'\x7fELFserver'), ('gamestop', b'\x7fELFcli')])
        self.assertEqual((self.root / 'current').resolve().name, 'a' * 40)

    def test_gzip_success(self):
        self.run_receiver([('server', b'\x7fELFserver'), ('gamestop', b'\x7fELFcli')], compressed=True)
        self.assertEqual((self.root / 'current').resolve().name, 'a' * 40)

    def test_gzip_extra_member_rejected(self):
        with self.assertRaises(ValueError):
            self.run_receiver([('server', b'\x7fELF'), ('gamestop', b'\x7fELF'), ('extra', b'x')], compressed=True)
        self.assertFalse((self.root / 'current').exists())

    def test_decompression_bomb_bound(self):
        source = gzip.GzipFile(fileobj=io.BytesIO(gzip.compress(b'x' * 4096)))
        bounded = receive.BoundedReader(source, limit=1024)
        with self.assertRaises(ValueError):
            bounded.read(4096)

    def test_oversize_member_rejected_before_payload(self):
        member = tarfile.TarInfo('server')
        member.size = 128 * 1024 * 1024 + 1
        stdin = type('Input', (), {'buffer': io.BytesIO(gzip.compress(member.tobuf()))})()
        with patch.object(receive, 'ROOT', self.root), patch.object(receive.sys, 'stdin', stdin), \
             patch.dict(os.environ, SSH_ORIGINAL_COMMAND='deploy ' + 'a' * 40):
            with self.assertRaisesRegex(ValueError, 'Invalid binary size'):
                receive.main()

    def test_rollback_preserves_data(self):
        old = self.root / 'releases' / ('b' * 40)
        old.mkdir()
        (self.root / 'current').symlink_to(old)
        with self.assertRaises(subprocess.CalledProcessError):
            self.run_receiver([('server', b'\x7fELFserver'), ('gamestop', b'\x7fELFcli')], failure=True)
        self.assertEqual((self.root / 'current').resolve(), old)

    def test_reject_traversal_and_duplicate_and_missing(self):
        for entries in [[('../evil', b'\x7fELF')], [('server', b'\x7fELF')],
                        [('server', b'\x7fELF'), ('server', b'\x7fELF')]]:
            with self.assertRaises(ValueError):
                self.run_receiver(entries)
            self.assertFalse((self.root / 'current').exists())

    def test_reject_shell(self):
        with self.assertRaises(ValueError):
            self.run_receiver([], command='deploy ' + 'a' * 40 + '; id')


if __name__ == '__main__':
    unittest.main()
