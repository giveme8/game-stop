#!/usr/bin/env python3
"""Compare two instances' public catalogs by ID, size and SHA-256.

Read-only and anonymous: it uses only what each instance already publishes to
any client. Byte-level verification downloads are deliberately left to the
migration tool, which can hash the published objects.
"""
import json
import sys
import time
import urllib.request

FETCH_ATTEMPTS = 6
# Some edges reject the default Python user agent outright; identify the tool.
USER_AGENT = 'game-stop-catalog-compare/0.1 (+https://github.com/giveme8/game-stop)'


def fetch(url: str) -> dict:
    last = None
    for attempt in range(1, FETCH_ATTEMPTS + 1):
        try:
            request = urllib.request.Request(url, headers={'User-Agent': USER_AGENT, 'Accept': 'application/json'})
            with urllib.request.urlopen(request, timeout=45) as response:
                return json.loads(response.read().decode())
        except Exception as error:  # transient edge/network failures are expected here
            last = error
            time.sleep(min(20, 3 * attempt))
    raise SystemExit(f'could not read {url}: {last}')


def index(origin: str) -> dict:
    catalog = fetch(origin.rstrip('/') + '/gamesource/v1/index.json')
    if catalog.get('protocol') != 'gamesource/1':
        raise SystemExit(f'{origin}: unexpected protocol {catalog.get("protocol")}')
    return {game['id']: game for game in catalog.get('games', [])}


def main() -> int:
    source = index(sys.argv[1] if len(sys.argv) > 1 else 'https://game-stop.baobaobao.online')
    target = index(sys.argv[2] if len(sys.argv) > 2 else 'https://game-stop.giveme8.com')
    missing, mismatched, ok = [], [], []
    for game_id, game in sorted(source.items()):
        mirror = target.get(game_id)
        if mirror is None:
            missing.append(game_id)
            continue
        same = (mirror['package']['sha256'] == game['package']['sha256']
                and mirror['package']['size'] == game['package']['size'])
        (ok if same else mismatched).append(game_id)
        if not same:
            print(f'  {game_id}: source {game["package"]["sha256"][:12]}/{game["package"]["size"]} '
                  f'!= target {mirror["package"]["sha256"][:12]}/{mirror["package"]["size"]}')
    extra = sorted(set(target) - set(source))
    print(json.dumps({
        'source_games': len(source), 'target_games': len(target),
        'matched': len(ok), 'missing': missing, 'mismatched': mismatched, 'extra_in_target': extra,
        'covers_present': sum(1 for game_id in ok if target[game_id].get('cover')),
    }, ensure_ascii=False, indent=2))
    return 1 if missing or mismatched else 0


if __name__ == '__main__':
    raise SystemExit(main())
