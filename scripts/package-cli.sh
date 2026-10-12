#!/usr/bin/env bash
# Package a public CLI with its legal texts and corresponding MPL driver source.
set -euo pipefail
cd "$(dirname "$0")/.."
: "${GOOS:?set GOOS}" "${GOARCH:?set GOARCH}"
version=${BUILD_VERSION:-dev}
stem="gamestop-${GOOS}-${GOARCH}"
ext=''
if [[ "$GOOS" == windows ]]; then ext='.exe'; fi
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "dist" "$stage/$stem/third_party/source"
root=$PWD
(
  cd server
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.buildVersion=$version" -o "$stage/$stem/gamestop$ext" ./cmd/gamestop
  go mod download -json github.com/go-sql-driver/mysql > "$stage/mysql-source.json"
)
cp LICENSE NOTICE THIRD_PARTY_NOTICES.md "$stage/$stem/"
cp -R third_party/licenses "$stage/$stem/third_party/"
python3 - "$stage/mysql-source.json" "$stage/$stem" <<'PY'
import json, pathlib, shutil, subprocess, sys
module = json.loads(pathlib.Path(sys.argv[1]).read_text())
if module.get('Error') or not module.get('Zip'):
    raise SystemExit('Corresponding MPL driver source is unavailable')
root = pathlib.Path(sys.argv[2])
shutil.copyfile(module['Zip'], root / 'third_party/source' / ('go-sql-driver-mysql-' + module['Version'] + '.zip'))
gomod = subprocess.check_output(['go', 'version', '-m', str(next(root.glob('gamestop*')))], text=True)
(root / 'BUILD_INFO.txt').write_text(subprocess.check_output(['go', 'version'], text=True) + '\n' + gomod)
goroot = pathlib.Path(subprocess.check_output(['go', 'env', 'GOROOT'], text=True).strip())
license_file = next((p for p in [goroot / 'LICENSE', goroot.parent / 'LICENSE'] if p.is_file()), None)
if license_file is None:
    raise SystemExit('Go runtime license was not found')
dest = root / 'third_party/go-runtime'
dest.mkdir(parents=True)
shutil.copyfile(license_file, dest / 'LICENSE')
if (goroot / 'PATENTS').is_file():
    shutil.copyfile(goroot / 'PATENTS', dest / 'PATENTS')
for subtree in [goroot / 'src/vendor', goroot / 'src/crypto/internal/boring']:
    if not subtree.exists():
        continue
    for p in subtree.rglob('*'):
        if p.is_file() and p.name.lower().startswith(('license', 'notice', 'copying', 'copyright', 'patents')):
            target = dest / p.relative_to(goroot)
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(p, target)
PY
tar -czf "dist/$stem.tar.gz" -C "$stage" "$stem"
printf 'Packaged %s\n' "dist/$stem.tar.gz"
