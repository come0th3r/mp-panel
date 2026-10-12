#!/usr/bin/env python3
"""Inspect or remove MP Panel uploads older than 24 hours. Python 3, Linux, Docker."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import time


def run(*args, data=None):
    return subprocess.check_output(args, input=data, text=True)


def inspect(identifier):
    return json.loads(run('docker', 'inspect', identifier))[0]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory', default='/opt/mp-panel')
    parser.add_argument('--project', default='mp-panel')
    parser.add_argument('--apply', action='store_true', help='Stop API/worker briefly and delete old upload copies')
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error('Run with sudo/root')
    if not re.fullmatch('[a-z0-9][a-z0-9_-]*', args.project):
        parser.error('Invalid Compose project')
    directory = Path(args.directory)
    if not directory.is_absolute() or directory.resolve() != directory or directory == Path('/'):
        parser.error('Installation directory must be an absolute path without links')
    services = {}
    for name in ('api', 'worker', 'postgres'):
        ids = run('docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + args.project,
                  '--filter', 'label=com.docker.compose.service=' + name).split()
        if len(ids) != 1:
            raise RuntimeError('Expected one container for ' + name)
        services[name] = inspect(ids[0])
        if services[name]['Config']['Labels'].get('com.docker.compose.project.working_dir') != str(directory):
            raise RuntimeError('Container belongs to another installation')
        state = services[name]['State']
        if state['Paused'] or state['Restarting']:
            raise RuntimeError('Container is paused or restarting: ' + name)
    if not services['postgres']['State']['Running']:
        raise RuntimeError('PostgreSQL must be running')
    env = dict(value.split('=', 1) for value in services['postgres']['Config']['Env'] if '=' in value)
    db, user = env.get('POSTGRES_DB', 'mp'), env.get('POSTGRES_USER', 'mp')
    volume = next((m['Name'] for m in services['api']['Mounts']
                   if m['Destination'] == '/var/lib/mp' and m['Type'] == 'volume'), None)
    if not volume or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9_.-]*', volume):
        raise RuntimeError('Expected named panel-data volume at /var/lib/mp')
    image = services['api']['Image']

    def sql(query):
        return run('docker', 'exec', '-i', services['postgres']['Id'], 'psql', '-X', '-v', 'ON_ERROR_STOP=1',
                   '-U', user, '-d', db, '-At', data=query)

    def helper(script, data=None, readonly=True):
        mount = 'type=volume,src=' + volume + ',dst=/data' + (',readonly' if readonly else '')
        return run('docker', 'run', '--rm', '-i', '--network', 'none', '--user', '0:0', '--entrypoint', 'sh',
                   '--mount', mount, image, '-c', script, data=data)

    def candidates():
        rows = [json.loads(line) for line in sql("SELECT jsonb_build_object('id',id,'path',payload->>'path',"
                    "'old',created_at<now()-interval '24 hours') FROM mp_entities WHERE kind='upload';").splitlines()]
        records = {row['id']: row for row in rows}
        listing = helper("set -eu; test ! -L /data/uploads; if [ -d /data/uploads ]; then "
                         "find /data/uploads -mindepth 1 -maxdepth 1 -type f -printf '%f %s %T@ %n\\n'; fi")
        selected = []
        for line in listing.splitlines():
            match = re.fullmatch(r'([a-f0-9]{32}) (\d+) ([0-9.]+) 1', line)
            if not match:
                continue
            identifier, size, modified = match.groups()
            record = records.get(identifier)
            if record:
                eligible = record['old'] and record['path'] == '/var/lib/mp/uploads/' + identifier
            else:
                eligible = float(modified) < time.time() - 86400
            if eligible:
                selected.append((identifier, int(size), record is not None))
        return selected

    restarted = []
    lock = None
    try:
        if args.apply:
            root = Path('/var/lib/mp-panel-updates') / args.project
            if root.resolve() != root:
                raise RuntimeError('Unsafe update lock directory')
            root.mkdir(mode=0o700, parents=True, exist_ok=True)
            if root.stat().st_uid != 0 or root.stat().st_mode & 0o022:
                raise RuntimeError('Update lock directory must be owned by root and not writable by others')
            lock_path = root / 'update.lock'
            lock_fd = os.open(lock_path, os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
            lock = os.fdopen(lock_fd, 'w')
            if os.fstat(lock_fd).st_uid != 0 or os.fstat(lock_fd).st_nlink != 1:
                raise RuntimeError('Unsafe update lock file')
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            for name in ('api', 'worker'):
                container = services[name]
                if container['State']['Running']:
                    restarted.append(container['Id'])
                    run('docker', 'stop', '-t', '60', container['Id'])
            for name in ('api', 'worker'):
                if inspect(services[name]['Id'])['State']['Running']:
                    raise RuntimeError('API/worker must be stopped before cleanup')
            active = sql("SELECT (SELECT count(*) FROM mp_commands WHERE state IN ('pending','delivered'))"
                         "+(SELECT count(*) FROM mp_work WHERE state IN ('queued','running'));").strip()
            if active != '0':
                raise RuntimeError('Unfinished jobs exist; no uploads deleted. Wait for completion and retry')
        selected = candidates()
        total = sum(size for _, size, _ in selected)
        print('Old upload copies: %d files, %.2f GiB (%d bytes)' % (len(selected), total / 2**30, total), flush=True)
        if not args.apply:
            print('Inspection only. Use --apply to delete these copies; failed transfers must be uploaded again.')
            return
        for offset in range(0, len(selected), 500):
            batch = selected[offset:offset + 500]
            # All names are validated 32-character IDs. Refuse links and nonregular files.
            helper('set -eu; test ! -L /data/uploads; while IFS= read -r id; do '
                   'file=/data/uploads/$id; test ! -L "$file"; '
                   'if [ -e "$file" ]; then test -f "$file"; test "$(stat -c %h "$file")" = 1; '
                   'rm -- "$file"; fi; done', data=''.join(identifier + '\n' for identifier, _, _ in batch), readonly=False)
            ids = ','.join("'" + identifier + "'" for identifier, _, registered in batch if registered)
            if ids:
                sql("DELETE FROM mp_entities WHERE kind='upload' AND id IN (" + ids + ");")
        print('Deleted only old upload copies. Keys, database, game files and update backups preserved.', flush=True)
    finally:
        restart_errors = []
        for identifier in restarted:
            try:
                run('docker', 'start', identifier)
            except subprocess.CalledProcessError as error:
                restart_errors.append(error)
        if lock:
            lock.close()
        if restart_errors:
            raise RuntimeError('Could not restart all API/worker containers; inspect docker compose ps') from restart_errors[0]


if __name__ == '__main__':
    main()
