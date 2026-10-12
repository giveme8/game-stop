#!/usr/bin/env python3
"""Verify public delivery of one instance: signed URLs plus byte integrity.

Reads only what the instance already publishes anonymously. Downloads the
largest public package and re-hashes it, so a passing run proves that the URL
in the catalog really returns the bytes the catalog promises.

Usage: verify-delivery.py [origin]
"""
from __future__ import annotations

import hashlib
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

USER_AGENT = 'game-stop-delivery-verify/0.1 (+https://github.com/giveme8/game-stop)'
SIGNED_HOST = '.r2.cloudflarestorage.com'
ATTEMPTS = 8


def fetch(url: str, limit: int | None = None, digest=None):
    """GET with retries; short/edge-truncated reads are retried too."""
    last = None
    for attempt in range(1, ATTEMPTS + 1):
        try:
            request = urllib.request.Request(url, headers={'User-Agent': USER_AGENT, 'Accept': 'application/json, */*'})
            with urllib.request.urlopen(request, timeout=90) as response:
                size = 0
                while True:
                    chunk = response.read(262144)
                    if not chunk:
                        break
                    size += len(chunk)
                    if digest is not None:
                        digest.update(chunk)
                    if limit is not None and size >= limit:
                        return size
                return size
        except Exception as error:  # edge resets and timeouts show up here
            last = error
            time.sleep(min(20, 2 * attempt))
    raise SystemExit(f'could not read {url}: {type(last).__name__}: {last}')


def read_json(url: str) -> dict:
    last = None
    for attempt in range(1, ATTEMPTS + 1):
        try:
            request = urllib.request.Request(url, headers={'User-Agent': USER_AGENT, 'Accept': 'application/json'})
            with urllib.request.urlopen(request, timeout=90) as response:
                raw = response.read()
            return json.loads(raw.decode())
        except Exception as error:
            last = error
            time.sleep(min(20, 2 * attempt))
    raise SystemExit(f'could not parse {url}: {type(last).__name__}: {last}')


def main() -> int:
    origin = (sys.argv[1] if len(sys.argv) > 1 else 'https://game-stop.giveme8.com').rstrip('/')
    catalog = read_json(origin + '/gamesource/v1/index.json')
    games = [game for game in catalog.get('games', []) if game.get('package')]
    if not games:
        raise SystemExit(f'{origin}: catalog has no games')
    signed = [game for game in games if SIGNED_HOST in urllib.parse.urlparse(game['package']['url']).netloc]
    covers = [game for game in games if SIGNED_HOST in urllib.parse.urlparse(game.get('cover') or '').netloc]
    print(json.dumps({
        'origin': origin, 'games': len(games),
        'packages_delivered_by_signed_url': len(signed),
        'covers_delivered_by_signed_url': len(covers),
    }, indent=2))

    largest = max(games, key=lambda game: game['package']['size'])
    digest = hashlib.sha256()
    started = time.time()
    size = fetch(largest['package']['url'], digest=digest)
    elapsed = max(time.time() - started, 0.001)
    result = {
        'game': largest['id'],
        'bytes': size,
        'expected_bytes': largest['package']['size'],
        'sha256': digest.hexdigest(),
        'sha256_matches_catalog': digest.hexdigest() == largest['package']['sha256'],
        'seconds': round(elapsed, 2),
        'MiB_per_second': round(size / 1024 / 1024 / elapsed, 2),
    }
    print(json.dumps(result, indent=2))
    ok = (result['sha256_matches_catalog'] and result['bytes'] == result['expected_bytes']
          and len(signed) == len(games))
    print('delivery verification:', 'PASS' if ok else 'FAIL')
    return 0 if ok else 1


if __name__ == '__main__':
    raise SystemExit(main())
