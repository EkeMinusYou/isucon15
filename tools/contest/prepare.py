import hashlib
import json
from pathlib import Path
import secrets

root = Path(__file__).resolve().parents[2]
manifest = json.loads((root / 'tools/contest/static-manifest.json').read_text())
for name, expected in manifest.items():
    actual = hashlib.sha256((root / 'public' / name).read_bytes()).hexdigest()
    if actual != expected['sha256']:
        raise SystemExit(f'static asset differs: {name}')
# This key only authenticates internal forwarding through the official router.
key = root / 'generated/isureal-transport.key'
key.parent.mkdir(exist_ok=True)
if not key.exists():
    key.write_text(secrets.token_hex(32))
    key.chmod(0o600)
for p in (root / 'data/initial/images').glob('*.bin'):
    if p.stat().st_size == 0:
        raise SystemExit(f'empty image part: {p}')
if not list((root / 'data/initial/images').glob('*.bin')):
    raise SystemExit('initial image bytes are missing')
print('initial inputs and transport credential ready')
