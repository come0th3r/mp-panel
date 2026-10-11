#!/usr/bin/env python3
"""Build release assets from committed sources, using Git and Python 3."""
import argparse
import gzip
import hashlib
import io
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--version', default='v0.3.4')
parser.add_argument('--ref', default='HEAD')
parser.add_argument('--output', default='release')
args = parser.parse_args()
if not re.fullmatch(r'v\d+\.\d+\.\d+(?:[-+][A-Za-z0-9.-]+)?', args.version):
    parser.error('version must be a release tag such as v0.3.4')

root = Path(__file__).resolve().parents[1]
commit = subprocess.check_output(['git', 'rev-parse', '--verify', args.ref + '^{commit}'], cwd=root, text=True).strip()
archive = subprocess.check_output(['git', 'archive', '--format=tar', commit], cwd=root)
allowed_roots = {'apps', 'cmd', 'db', 'deploy', 'docs', 'internal', 'scripts'}
allowed_files = {'.dockerignore', '.env.example', '.gitattributes', '.gitignore', 'README.md', 'Dockerfile', 'compose.yaml', 'go.mod', 'go.sum', 'LICENSE', 'LICENSE.md', 'CHANGELOG.md'}
with tarfile.open(fileobj=io.BytesIO(archive), mode='r:') as source:
    for entry in source:
        path = PurePosixPath(entry.name)
        if path.is_absolute() or '..' in path.parts or not (entry.isfile() or entry.isdir()):
            raise SystemExit('Unsafe source archive entry: ' + entry.name)
        if path.parts[0] not in allowed_roots and entry.name not in allowed_files:
            raise SystemExit('Unexpected source archive entry: ' + entry.name)
        if path.name == '.env' or path.suffix in {'.key', '.pem', '.p12', '.pfx'}:
            raise SystemExit('Secret file in source archive: ' + entry.name)
        if path.parts[0] == 'docs' and entry.isfile() and entry.name not in {'docs/DEPLOYMENT.md', 'docs/GATEWAY.md', 'docs/API_CONTRACT.md', 'docs/GLOSSARY.md'}:
            raise SystemExit('Unexpected documentation file: ' + entry.name)
    installers = {name: source.extractfile('deploy/' + name).read() for name in ('setup.sh', 'setup.ps1')}

output = Path(args.output).resolve()
output.mkdir(parents=True, exist_ok=True)
assets = []
bundle = output / ('mp-panel-' + args.version + '.tar.gz')
with bundle.open('wb') as raw:
    with gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0) as compressed:
        compressed.write(archive)
assets.append(bundle)
for name, content in installers.items():
    asset = output / name
    asset.write_bytes(content)
    assets.append(asset)
sums = ''.join(hashlib.sha256(asset.read_bytes()).hexdigest() + '  ' + asset.name + '\n' for asset in assets)
(output / 'SHA256SUMS').write_text(sums, encoding='ascii', newline='\n')
print('Source commit: ' + commit)
for asset in assets + [output / 'SHA256SUMS']:
    print(str(asset))
