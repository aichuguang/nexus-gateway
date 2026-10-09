#!/usr/bin/env python3
"""Bounded, local-only release gate. Never authenticates or writes gateway state."""
import hashlib
import json
import pathlib
import re
import subprocess
import sys
import time


def command(*args):
    return subprocess.check_output(args, timeout=15, stderr=subprocess.STDOUT).decode().strip()


def verify(binary, data, listen, expected_sha, manager=None, proxy=None):
    binary, data = pathlib.Path(binary), pathlib.Path(data)
    # Four-argument invocations from older deployments keep working. Scripts
    # also pass explicit unit names so rollback checks the selected layout.
    app_name = 'w103d-gateway' if binary.name == 'w103d-gateway' else 'gateway'
    manager = manager or app_name + '.service'
    proxy = proxy or app_name + '-proxy.service'
    if hashlib.sha256(binary.read_bytes()).hexdigest() != expected_sha:
        raise RuntimeError('installed binary hash does not match release')
    expected = json.loads(command(str(binary), 'version', '--json'))
    command(str(binary), 'state-check', '--data-dir', str(data))
    config = (data / 'managed-nginx.conf').read_text(encoding='utf-8')
    generation = re.search(r'return 200 "([a-f0-9]{64})\\n";', config)
    if not generation:
        raise RuntimeError('managed nginx generation is absent')
    last_error = 'health check not attempted'
    for _ in range(12):
        try:
            command('systemctl', 'is-active', '--quiet', manager)
            command('systemctl', 'is-active', '--quiet', proxy)
            health = json.loads(command('curl', '--noproxy', '*', '-fsS', '--max-time', '3', 'http://' + listen + '/healthz'))
            if health.get('status') != 'ok' or any(health.get(key) != expected.get(key) for key in ('version', 'commit', 'buildTime', 'dirty')):
                raise RuntimeError('health endpoint serves a different build')
            actual = command('curl', '--noproxy', '*', '-fsS', '--max-time', '3', 'http://127.0.0.1:18079/__gateway_generation')
            if actual != generation.group(1):
                raise RuntimeError('nginx workers do not serve the installed generation')
            # Catch an immediately exiting process; a single active sample is insufficient.
            time.sleep(1)
            command('systemctl', 'is-active', '--quiet', manager)
            command('systemctl', 'is-active', '--quiet', proxy)
            print('Verified binary, state, build identity, health and nginx generation')
            return
        except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
            last_error = str(error)
            time.sleep(1)
    raise RuntimeError('release health deadline exceeded: ' + last_error)


if __name__ == '__main__':
    try:
        verify(*sys.argv[1:])
    except Exception as error:
        print('RELEASE GATE FAILED: ' + str(error), file=sys.stderr)
        sys.exit(1)
