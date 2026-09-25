#!/usr/bin/env python3
"""Exercise the installed-config default and browser opener without a real desktop."""
import json, os, pathlib, socket, subprocess, tempfile, time, urllib.request
root = pathlib.Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory() as tmp:
    home = pathlib.Path(tmp)
    cfgdir = home / '.config/nexaroute'
    cfgdir.mkdir(parents=True)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    cfg = json.loads((root / 'configs/config.example.json').read_text())
    cfg['listen'] = f'127.0.0.1:{port}'
    cfg['providers'] = []
    cfg['probe']['enabled'] = False
    (cfgdir / 'config.json').write_text(json.dumps(cfg))
    marker = home / 'opened-url'
    fake = home / 'google-chrome'
    fake.write_text('#!/bin/sh\nprintf "%s" "$1" > "$NEXAROUTE_BROWSER_MARKER"\n')
    fake.chmod(0o755)
    env = dict(os.environ, HOME=str(home), DISPLAY=':test', PATH=str(home)+':'+os.environ['PATH'], NEXAROUTE_BROWSER_MARKER=str(marker))
    for key in ['NEXAROUTE_CONFIG','NEXAROUTE_LISTEN','NEXAROUTE_NO_BROWSER']:
        env.pop(key, None)
    url = f'http://127.0.0.1:{port}/'
    for args in [[], ['-no-browser']]:
        marker.unlink(missing_ok=True)
        proc = subprocess.Popen([str(root/'bin/nexaroute-linux-amd64'), *args], cwd=home, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(80):
                assert proc.poll() is None, 'gateway exited'
                try:
                    with urllib.request.urlopen(url+'healthz', timeout=.2) as r:
                        assert r.status == 200
                    if args or marker.exists():
                        break
                except OSError:
                    pass
                time.sleep(.1)
            else:
                raise AssertionError('launcher timeout')
            if args:
                time.sleep(.2)
                assert not marker.exists(), '-no-browser ignored'
            else:
                assert marker.read_text() == url, 'wrong browser URL'
            assert not (home/'config.json').exists(), 'created unrelated working-directory config'
        finally:
            proc.terminate()
            proc.wait(timeout=10)
print('LAUNCHER PASS: user config, Chrome URL, headless opt-out')
