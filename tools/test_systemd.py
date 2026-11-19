"""Disposable Linux container test; never run against the host service manager.

Example (existing image with Python and systemd-analyze, native CPU binary):
docker run --rm --network none --user 0 -e SCANSPOOL_SYSTEMD_CONTAINER_TEST=1 \
  -v "$PWD:/src:ro" --entrypoint python3 \
  mcr.microsoft.com/playwright:v1.63.0-noble \
  /src/tools/test_systemd.py /src/bin/scanspool-linux-arm64

PID 1 advertises systemd for the platform check. Fake systemctl starts only this
test's real receiver, under an unprivileged account, and never calls systemd.
"""

import ctypes
import json
import os
from pathlib import Path
import pwd
import signal
import subprocess
import sys
import time

from test_terminal import Terminal


STATE = Path('/tmp/scanspool-fake-systemd.json')
MANIFEST = Path('/etc/scanspool/service.json')
UNIT = Path('/etc/systemd/system/scanspool.service')
INSTALLED = '/usr/local/bin/scanspool'
LOG = Path('/tmp/scanspool-test-journal')


def alive(pid):
    path = Path(f'/proc/{pid}/stat')
    return pid > 0 and path.exists() and path.read_text().split()[2] != 'Z'


def fake_systemctl(args):
    assert Path('/.dockerenv').exists(), 'Container only'
    action = args[0]
    if action == 'daemon-reload':
        return
    assert args[1] == UNIT.name, args
    state = json.loads(STATE.read_text()) if STATE.exists() else {'pid': 0, 'enabled': False}
    if action == 'show':
        print('LoadState=' + ('loaded' if UNIT.exists() else 'not-found'))
        print('FragmentPath=' + (str(UNIT) if UNIT.exists() else ''))
        print('ActiveState=' + ('active' if alive(state['pid']) else 'inactive'))
        print('UnitFileState=' + ('enabled' if state['enabled'] else 'disabled'))
        return
    if action in ('stop', 'restart') and alive(state['pid']):
        # Exact PID was created by this harness; never match process names.
        os.kill(state['pid'], signal.SIGTERM)
        deadline = time.monotonic() + 10
        while alive(state['pid']):
            assert time.monotonic() < deadline, 'Receiver did not stop'
            time.sleep(.05)
    if action in ('start', 'restart'):
        assert not alive(state['pid']), 'Second receiver requested'
        manifest = json.loads(MANIFEST.read_text())
        env = dict(os.environ, HOME=manifest['home'])
        with LOG.open('ab') as log:
            process = subprocess.Popen(
                [INSTALLED, 'run', '--data-dir', manifest['dataDir']],
                user=int(manifest['uid']), group=int(manifest['gid']), extra_groups=[],
                env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                start_new_session=True, umask=0o022,
            )
        state['pid'] = process.pid
    elif action == 'enable':
        state['enabled'] = True
    elif action == 'disable':
        state['enabled'] = False
    else:
        assert action == 'stop', action
    STATE.write_text(json.dumps(state))


def check(binary):
    assert os.getpid() == 1 and os.geteuid() == 0 and Path('/.dockerenv').exists()
    assert os.environ.get('SCANSPOOL_SYSTEMD_CONTAINER_TEST') == '1'
    assert not MANIFEST.exists(), 'Fresh disposable container required'
    assert ctypes.CDLL(None).prctl(15, b'systemd', 0, 0, 0) == 0
    os.umask(0o077)  # Administrator defaults must not hide the public manifest.
    account = pwd.getpwuid(1000)
    directory = Path(account.pw_dir) / "scansnap service'$docs"
    directory.mkdir(mode=0o700)
    os.chown(directory, account.pw_uid, account.pw_gid)
    fake_bin = Path('/usr/local/bin')
    for name, mode in [('systemctl', '--fake-systemctl'), ('journalctl', '--fake-journal')]:
        wrapper = fake_bin / name
        wrapper.write_text(f'#!/bin/sh\nexec {sys.executable} /src/tools/test_systemd.py {mode} "$@"\n')
        wrapper.chmod(0o755)
    os.environ['PATH'] = '/usr/local/bin:/usr/bin:/bin'

    def run(args, receiver=False, expected=0):
        options = {}
        if receiver:
            options = {'user': account.pw_uid, 'group': account.pw_gid, 'extra_groups': [],
                       'env': dict(os.environ, HOME=account.pw_dir)}
        result = subprocess.run(args, capture_output=True, text=True, timeout=40, **options)
        assert result.returncode == expected, (args, result.returncode, result.stdout, result.stderr)
        return result

    run([binary, 'service', 'install', '--user', account.pw_name, '--data-dir', str(directory)])
    assert not alive(json.loads(STATE.read_text())['pid']), 'Install started receiver'
    run(['/usr/bin/systemd-analyze', 'verify', '--man=no', str(UNIT)])
    manifest = json.loads(MANIFEST.read_text())
    run([INSTALLED, 'status', '--data-dir', str(directory)], expected=1)
    assert not (directory / 'service.lock').exists(), 'Root command mutated private data'

    command = [sys.executable, '/src/tools/test_systemd.py', '--as-user',
               str(account.pw_uid), str(account.pw_gid), INSTALLED, 'setup']
    terminal = Terminal(INSTALLED, str(directory), command=command)
    try:
        assert terminal.finish() == 1
        assert b'service start' in terminal.output
    finally:
        terminal.close()

    run([INSTALLED, 'service', 'start'])
    pid = json.loads(STATE.read_text())['pid']
    assert alive(pid)
    status = json.loads(run([INSTALLED, 'status', '--json'], receiver=True).stdout)
    assert status['running'] and not status['configured']
    run([INSTALLED, 'service', 'start'])
    assert json.loads(STATE.read_text())['pid'] == pid, 'Repeated start changed process'
    terminal = Terminal(INSTALLED, str(directory), command=command)
    try:
        terminal.wait('Start scan')
        terminal.send(b'\x03')
        assert terminal.finish() == 130
        assert alive(pid), 'Closing attached TUI stopped service'
    finally:
        terminal.close()

    run([binary, 'service', 'install'])
    assert json.loads(STATE.read_text())['pid'] == pid, 'Upgrade restarted receiver'
    assert json.loads(MANIFEST.read_text()) == manifest
    run([INSTALLED, 'service', 'logs'])
    run([INSTALLED, 'service', 'restart'])
    assert json.loads(STATE.read_text())['pid'] != pid
    run([INSTALLED, 'service', 'stop'])
    assert json.loads(run([INSTALLED, 'service', 'status', '--json']).stdout)['enabled']
    run([INSTALLED, 'service', 'uninstall'])
    assert directory.exists() and not MANIFEST.exists() and not UNIT.exists()
    assert not Path(INSTALLED).exists()
    assert directory.stat().st_uid == account.pw_uid
    print('Linux service CLI: install, unit verification, UID guard, TUI attach/detach, '
          'readiness, repeated start, upgrade, restart, stop and uninstall passed')


if __name__ == '__main__':
    if sys.argv[1] == '--fake-systemctl':
        fake_systemctl(sys.argv[2:])
    elif sys.argv[1] == '--fake-journal':
        assert Path('/.dockerenv').exists()
        print(LOG.read_text() if LOG.exists() else '')
    elif sys.argv[1] == '--as-user':
        assert Path('/.dockerenv').exists()
        uid, gid = map(int, sys.argv[2:4])
        os.environ['HOME'] = pwd.getpwuid(uid).pw_dir
        os.setgroups([])
        os.setgid(gid)
        os.setuid(uid)
        os.execv(sys.argv[4], sys.argv[4:])
    else:
        check(*sys.argv[1:])
