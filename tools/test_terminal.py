"""PTY integration checks for a ScanSpool binary; never contacts a scanner."""

import argparse
import codecs
import re
import unicodedata
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import signal
import struct
import subprocess
import tempfile
import termios
import time


class Screen:
    """Small VT cursor/erase/line decoder for Bubble Tea's delta renderer."""
    def __init__(self):
        self.cells = [[' '] * 160 for _ in range(60)]
        self.row = self.col = 0
        self.pending = ''
        self.unhandled = set()
        self.decoder = codecs.getincrementaldecoder('utf-8')()

    def feed(self, data):
        self.pending += self.decoder.decode(data)
        while self.pending:
            if self.pending.startswith('\x1b['):
                match = re.match(r'\x1b\[([0-?]*)([ -/]*)([@-~])', self.pending)
                if not match:
                    return
                self.pending = self.pending[match.end():]
                raw, _, action = match.groups()
                if raw.startswith('?') or raw.startswith('>') or raw.startswith('='):
                    if raw == '?1049' and action == 'h':
                        self.cells = [[' '] * 160 for _ in range(60)]
                        self.row = self.col = 0
                    continue
                args = [int(v or 0) for v in raw.split(';')] if raw else [0]
                n = args[0] or 1
                if action in ('H', 'f'):
                    self.row = max(0, (args[0] or 1) - 1)
                    self.col = max(0, (args[1] if len(args) > 1 and args[1] else 1) - 1)
                elif action == 'A': self.row = max(0, self.row - n)
                elif action == 'B': self.row += n
                elif action == 'C': self.col += n
                elif action == 'D': self.col = max(0, self.col - n)
                elif action == 'd': self.row = n - 1
                elif action == 'G': self.col = n - 1
                elif action == 'J':
                    if args[0] == 2: self.cells = [[' '] * 160 for _ in range(60)]
                    elif args[0] == 0:
                        self.cells[self.row][self.col:] = [' '] * (160 - self.col)
                        for row in range(self.row + 1, 60): self.cells[row] = [' '] * 160
                elif action == 'K':
                    if args[0] == 2: self.cells[self.row] = [' '] * 160
                    elif args[0] == 0: self.cells[self.row][self.col:] = [' '] * (160 - self.col)
                elif action == 'M':
                    count = min(n, 60 - self.row)
                    self.cells[self.row:] = self.cells[self.row + count:] + [[' '] * 160 for _ in range(count)]
                elif action == 'L':
                    count = min(n, 60 - self.row)
                    self.cells[self.row:] = [[' '] * 160 for _ in range(count)] + self.cells[self.row:60 - count]
                elif action not in ('m', 'h', 'l', 'n', 'c'):
                    self.unhandled.add((raw, action))
                self.row = min(59, self.row)
                self.col = min(159, self.col)
                continue
            if self.pending.startswith('\x1b'):
                if len(self.pending) < 2:
                    return
                self.pending = self.pending[2:]
                continue
            char, self.pending = self.pending[0], self.pending[1:]
            if char == '\r': self.col = 0
            elif char == '\n': self.row = min(59, self.row + 1)
            elif char >= ' ':
                self.cells[self.row][self.col] = char
                width = 0 if unicodedata.combining(char) else 2 if unicodedata.east_asian_width(char) in ('W', 'F') else 1
                self.col = min(159, self.col + width)

    def text(self):
        return '\n'.join(''.join(row).rstrip() for row in self.cells)


class Terminal:
    def __init__(self, binary, directory, command=None):
        self.master, slave = pty.openpty()
        self.slave = slave
        self.original_attributes = termios.tcgetattr(slave)
        fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack('HHHH', 28, 110, 0, 0))
        env = dict(os.environ, TERM='xterm-256color', LANG='en_US.UTF-8')
        for key in tuple(env):
            if key.startswith('SCANSPOOL_'):
                del env[key]
        self.process = subprocess.Popen(command or [binary, 'setup', '--data-dir', directory],
                                        stdin=slave, stdout=slave, stderr=slave, env=env, start_new_session=True)
        self.output = b''
        self.screen = Screen()

    def wait(self, text, start=0):
        deadline = time.monotonic() + 10
        while text not in self.screen.text():
            if time.monotonic() > deadline:
                raise RuntimeError('Terminal did not render ' + repr(text) + '\n' + self.screen.text() + '\nRAW: ' + repr(self.output[-1000:]))
            if select.select([self.master], [], [], .1)[0]:
                try:
                    data = os.read(self.master, 65536)
                    self.output += data
                    self.screen.feed(data)
                except OSError:
                    raise RuntimeError('Terminal ended early: ' + self.screen.text() + '\nRAW: ' + repr(self.output[-1000:])) from None
        return len(self.output)

    def drain(self):
        while select.select([self.master], [], [], .05)[0]:
            try:
                data = os.read(self.master, 65536)
            except OSError:
                return
            if not data:
                return
            self.output += data
            self.screen.feed(data)

    def send(self, value):
        marker = len(self.output)
        os.write(self.master, value)
        return marker

    def finish(self):
        deadline = time.monotonic() + 10
        while self.process.poll() is None:
            self.drain()
            if time.monotonic() > deadline:
                raise RuntimeError('TUI did not exit')
        self.drain()
        return self.process.returncode

    def restored(self):
        actual = termios.tcgetattr(self.slave)
        flags = termios.ECHO | termios.ICANON | termios.ISIG | termios.IEXTEN
        assert actual[3] & flags == self.original_attributes[3] & flags
        assert actual[:3] == self.original_attributes[:3]

    def close(self):
        if self.process.poll() is None:
            self.process.send_signal(signal.SIGTERM)
            deadline = time.monotonic() + 10
            while self.process.poll() is None and time.monotonic() < deadline:
                if select.select([self.master], [], [], .1)[0]:
                    try:
                        os.read(self.master, 65536)
                    except OSError:
                        break
            if self.process.poll() is None:
                self.process.kill()  # Only this harness's own exact child.
                self.process.wait(timeout=5)
        os.close(self.master)
        os.close(self.slave)


def check(binary):
    with tempfile.TemporaryDirectory(prefix='scanspool-pty-') as directory:
        receiver = subprocess.Popen([binary, 'run', '--data-dir', directory], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        terminal = None
        try:
            endpoint = Path(directory) / 'control.json'
            deadline = time.monotonic() + 10
            while not endpoint.exists():
                if receiver.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError('Receiver failed to start')
                time.sleep(.05)
            status = json.loads(subprocess.check_output([binary, 'status', '--data-dir', directory, '--json']))
            assert status['running'] and not status['configured']
            terminal = Terminal(binary, directory)
            version = subprocess.check_output([binary, '--version'], text=True).strip()
            terminal.wait(f'ScanSpool {version} · home')
            terminal.wait('Start scan')
            terminal.send(b'jjjjjj\r')
            terminal.wait('> Color mode: color')
            terminal.send(b'\x1b[C')
            terminal.wait('> Color mode: gray')
            terminal.send(b'\r')
            terminal.wait('Scan profile saved.')
            profile_cmd = [binary, 'profile']
            args = ['--data-dir', directory, '--json']
            profile = json.loads(subprocess.check_output([*profile_cmd, 'show', *args]))
            assert profile['ColorMode'] == 2, 'Attached TUI did not save the live profile'
            subprocess.check_call([*profile_cmd, 'set', *args, '--dpi', '200', '--sides', 'simplex'],
                                  stdout=subprocess.DEVNULL)
            terminal.send(b'jjjjjj\r')
            terminal.wait('Resolution (dpi): 200')
            terminal.wait('Sides: simplex')
            terminal.send(b'\x12')
            terminal.wait('Defaults loaded.')
            terminal.send(b'\r')
            terminal.wait('Scan profile saved.')
            profile = json.loads(subprocess.check_output([*profile_cmd, 'show', *args]))
            assert profile['ColorMode'] == 1 and profile['Quality'] == 3 and profile['Duplex']
            rejected_scan = subprocess.run([binary, 'scan', '--data-dir', directory], capture_output=True, text=True)
            assert rejected_scan.returncode != 0 and 'configure a scanner' in rejected_scan.stderr
            terminal.send(b'jjjjj\r')
            terminal.wait('configure a scanner with setup first')
            terminal.send(b'\x1b')
            terminal.wait('Draft discarded.')
            terminal.wait('Output destination')
            terminal.send(b'jj\r')
            terminal.wait('Local folder')
            terminal.send(b'\r')
            terminal.wait('New password (blank = keep)')
            terminal.send(b'127.0.0.1:2121\tscan\tsynthetic-secret\t\t\x15/consume\r')
            terminal.wait('Output destination saved.')
            assert b'synthetic-secret' not in terminal.output
            settings = Path(directory) / 'settings.json'
            saved = json.loads(settings.read_text())
            assert saved['host'] == '127.0.0.1:2121' and saved['password'] == 'synthetic-secret'
            terminal.send(b'jj\r')
            terminal.wait('Local folder')
            terminal.send(b'\r')
            terminal.wait('(keep existing)')
            terminal.send(b'\x15discarded-host\x1b')
            terminal.wait('Draft discarded.')
            assert json.loads(settings.read_text()) == saved
            terminal.send(b'jj\r')
            terminal.wait('Local folder')
            terminal.send(b'\x1b[B\r')
            terminal.wait('Local directory (absolute path)')
            local_directory = str(Path(directory) / 'finished' / 'scans')
            terminal.send(local_directory.encode() + b'\r')
            terminal.wait('Output destination saved.')
            local_settings = json.loads(settings.read_text())
            assert local_settings['kind'] == 'local' and local_settings['localDirectory'] == local_directory
            assert local_settings['host'] == saved['host'] and local_settings['password'] == saved['password']
            assert Path(local_directory).is_dir(), 'Saving local settings did not prepare destination'
            assert not list(Path(local_directory).iterdir()), 'Output check left temporary files'
            status = json.loads(subprocess.check_output([binary, 'status', '--data-dir', directory, '--json']))
            assert status['outputReady'] and status['outputKind'] == 'local' and not status['ftpReady']
            terminal.send(b'jj\r')
            terminal.wait('Local folder')
            terminal.send(b'\r')
            terminal.wait('Local directory (absolute path)')
            terminal.send(b'\x15discard-local\x1b')
            terminal.wait('Draft discarded.')
            assert json.loads(settings.read_text()) == local_settings
            if os.geteuid() != 0:
                readonly = Path(directory) / 'readonly'
                readonly.mkdir(mode=0o500)
                try:
                    terminal.send(b'jj\r')
                    terminal.wait('Local folder')
                    terminal.send(b'\r')
                    terminal.wait('Local directory (absolute path)')
                    terminal.send(b'\x15' + str(readonly).encode() + b'\r')
                    terminal.wait('permission denied')
                    assert json.loads(settings.read_text()) == local_settings, 'Invalid output overwrote saved settings'
                    terminal.send(b'\x1b')
                    terminal.wait('Draft discarded.')
                finally:
                    readonly.chmod(0o700)
                outbox = Path(directory) / 'outbox'
                outbox.mkdir(mode=0o700, exist_ok=True)
                queued_name = 'scan_access_test.pdf'
                queued_data = b'%PDF-1.7\ncomplete test document\n%%EOF\n'
                (outbox / queued_name).write_bytes(queued_data)
                destination = Path(local_directory)
                original_mode = destination.stat().st_mode & 0o777
                destination.chmod(0o500)
                try:
                    metadata = outbox / (queued_name + '.pending')
                    metadata.write_text(json.dumps({
                        'name': queued_name,
                        'target': {'kind': 'local', 'localDirectory': local_directory},
                    }))
                    metadata.replace(outbox / (queued_name + '.json'))
                    denied = subprocess.run([binary, 'retry', '--data-dir', directory], capture_output=True, text=True)
                    assert denied.returncode != 0 and 'permission denied' in denied.stderr
                    terminal.wait('Permission denied')
                    terminal.wait('1 PDF(s) in outbox')
                    assert (outbox / queued_name).read_bytes() == queued_data
                    assert 'Draft discarded.' in terminal.screen.text(), 'Prior toast hid error banner'
                finally:
                    destination.chmod(original_mode)
                subprocess.check_call([binary, 'retry', '--data-dir', directory])
                terminal.wait('Queued PDFs: 0')
                deadline = time.monotonic() + 10
                while 'Permission denied' in terminal.screen.text():
                    terminal.drain()
                    if time.monotonic() > deadline:
                        raise RuntimeError('Resolved delivery error remained visible\n' + terminal.screen.text() + '\nUnhandled: ' + repr(terminal.screen.unhandled) + '\nRAW: ' + repr(terminal.output[-1800:]))
                    time.sleep(.05)
                assert (destination / queued_name).read_bytes() == queued_data
                print('PTY: denied output rejected, queued PDF retained, persistent error shown and cleared after retry')
            terminal.send(b'\x03')
            assert terminal.finish() == 130
            terminal.restored()
            assert receiver.poll() is None, 'Closing attached setup stopped receiver'
            print('PTY: scan action/rejection, setup attach, FTP/local selection, secret masking, save, discard and terminal restoration passed')
        finally:
            if terminal is not None:
                terminal.close()
            if receiver.poll() is None:
                receiver.send_signal(signal.SIGTERM)
            receiver.wait(timeout=15)
            receiver.stderr.close()
        status = json.loads(subprocess.check_output([binary, 'status', '--data-dir', directory, '--json']))
        assert not status['running'] and not status['online'] and status['outputReady'] and not status['ftpReady']
        assert status['outputKind'] == 'local' and status['outputDirectory'] == local_directory
        assert not endpoint.exists()
        unavailable_scan = subprocess.run([binary, 'scan', '--data-dir', directory], capture_output=True, text=True)
        assert unavailable_scan.returncode != 0 and 'no running receiver' in unavailable_scan.stderr
        print('CLI: persisted settings and stopped-service status passed')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('binary')
    args = parser.parse_args()
    check(str(Path(args.binary).resolve()))
