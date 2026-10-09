#!/usr/bin/env python3
"""Exercise the real native bootstrap over verified local HTTPS, without Node/Go on PATH."""
import functools
import hashlib
import http.server
import json
import os
from pathlib import Path
import platform
import re
import shutil
import ssl
import subprocess
import sys
import tempfile
import threading

release_root = Path(sys.argv[1]).resolve()
versions = sys.argv[2:]
if len(versions) != 2:
    raise SystemExit('Usage: test-native-installer.py RELEASE_ROOT VERSION1 VERSION2')
bootstrap = Path(__file__).resolve().parent / 'install-native.sh'
repository = bootstrap.parent.parent
for version in versions:
    bundle = release_root / version
    rendered = (bundle / 'install.sh').read_text()
    subprocess.run(['/bin/sh', '-n', str(bundle / 'install.sh')], check=True)
    assert 'base=https://github.com/DanielNetzer/forgecell/releases/download' in rendered
    assert '@FORGECELL_' not in rendered, 'unresolved installer placeholder'
    assert f'version=${{FORGECELL_VERSION:-{version}}}' in rendered
    assert f'tag="v{version}"' in rendered
    assert (bundle / 'LICENSE').read_bytes() == (repository / 'LICENSE').read_bytes()
    assert (bundle / 'THIRD-PARTY-NOTICES.txt').stat().st_size > 1000
    for system in ['darwin', 'linux']:
        for arch in ['amd64', 'arm64']:
            binary = bundle / f'forgecell-{system}-{arch}'
            assert binary.with_name(binary.name + '.sha256').read_text().strip() == hashlib.sha256(binary.read_bytes()).hexdigest()
assert (release_root / 'install.sh').read_bytes() == (release_root / versions[-1] / 'install.sh').read_bytes()
print('PASS: all four packages, license, notices and rendered version/tag pins')
public_url = 'https://github.com/DanielNetzer/forgecell-releases/releases/download/0.2.0-preview.1/install.sh'
def documented_command(path):
    commands = [block.strip() for block in re.findall(r'```sh\n(.*?)\n```', path.read_text(), re.S) if public_url in block]
    assert len(commands) == 1, f'{path}: expected one public install command'
    assert '\n' not in commands[0], 'public install must remain one line'
    return commands[0]
command = documented_command(repository / 'README.md')
assert command == documented_command(repository / 'docs/install.md'), 'public commands differ'
stall_finished = threading.Event()
architecture = {'aarch64': 'arm64', 'arm64': 'arm64', 'x86_64': 'amd64'}[platform.machine()]
artifact = f'forgecell-{platform.system().lower().replace("darwin", "darwin")}-{architecture}'
class Quiet(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        self.server.requests.append(self.path)
        if self.path == '/empty.sh':
            self.send_response(200)
            self.send_header('Content-Length', '0')
            self.end_headers()
        elif self.path == '/stall.sh':
            self.send_response(200)
            self.send_header('Content-Length', '100000')
            self.end_headers()
            # Send executable partial content; it must never run after timeout.
            self.wfile.write(b'echo partial-bootstrap-executed >&2\n')
            self.wfile.flush()
            stall_finished.wait(40)
        elif self.path == self.server.partial_path:
            self.send_response(200)
            self.send_header('Content-Length', '100000')
            self.end_headers()
            self.wfile.write(b'partial binary')
            self.wfile.flush()
            self.close_connection = True
        else:
            super().do_GET()

    def log_message(self, *_args):
        pass

with tempfile.TemporaryDirectory(prefix='forgecell-install-test-') as temporary:
    root = Path(temporary)
    served = root / 'served'
    shutil.copytree(release_root, served)
    github = served / 'DanielNetzer' / 'forgecell' / 'releases' / 'download'
    github.mkdir(parents=True)
    for version in versions:
        (github / f'v{version}').symlink_to(served / version, target_is_directory=True)
    requests = []
    config = root / 'openssl.cnf'
    config.write_text('[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=ext\n[dn]\nCN=localhost\n[ext]\nsubjectAltName=DNS:localhost\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,digitalSignature,keyEncipherment\n')
    certificate, key = root / 'cert.pem', root / 'key.pem'
    subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-config', str(config), '-keyout', str(key), '-out', str(certificate)], check=True, capture_output=True)
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(Quiet, directory=str(served)))
    server.requests = requests
    server.partial_path = None
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(certificate, key)
    server.socket = tls.wrap_socket(server.socket, server_side=True)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        tools = root / 'tools'
        tools.mkdir()
        for name in ['sh', 'curl', 'uname', 'mktemp', 'chmod', 'cat', 'rm', 'shasum', 'sha256sum']:
            found = shutil.which(name)
            if found:
                (tools / name).symlink_to(found)
        home = root / "install with spaces'"
        bin_dir = root / 'bin'
        (home / 'labs' / 'existing' / 'ledgers').mkdir(parents=True)
        sentinel = home / 'labs' / 'existing' / 'ledgers' / 'keep.json'
        sentinel.write_text('{"preserve":true}')
        env = dict(os.environ, PATH=str(tools), HOME=str(root), FORGECELL_HOME=str(home), FORGECELL_BIN=str(bin_dir), FORGECELL_RELEASE_BASE=f'https://localhost:{server.server_port}', CURL_CA_BUNDLE=str(certificate))
        env.pop('FORGECELL_LAUNCHER', None)
        for name in ['GH_TOKEN', 'GITHUB_TOKEN', 'GH_ENTERPRISE_TOKEN', 'GITHUB_ENTERPRISE_TOKEN']:
            env.pop(name, None)
        download_tmp = root / 'download temporary files'
        download_tmp.mkdir()
        env['TMPDIR'] = str(download_tmp)
        real_curl = shutil.which('curl')
        (tools / 'curl').unlink()
        # Transport adapter: preserve the generated GitHub URL/path, route only
        # its origin to certificate-verified localhost. No production test hook.
        (tools / 'curl').write_text('#!/usr/bin/env python3\nimport os, sys\na = [x.replace("https://github.com", os.environ["FIXTURE_ORIGIN"]) for x in sys.argv[1:]]\nos.execv(' + repr(real_curl) + ', ["curl"] + a)\n')
        (tools / 'curl').chmod(0o700)
        (tools / 'python3').symlink_to(sys.executable)
        env['FIXTURE_ORIGIN'] = f'https://localhost:{server.server_port}'
        def public_install(version, path=None, successful=True, diagnostic=None):
            env.pop('FORGECELL_VERSION', None)
            env.pop('FORGECELL_RELEASE_BASE', None)
            path = path or f'DanielNetzer/forgecell/releases/download/v{version}/install.sh'
            local_command = command.replace(public_url, f'https://localhost:{server.server_port}/{path}')
            run = subprocess.run(['/bin/sh', '-c', local_command], cwd=root, env=env, text=True, capture_output=True, timeout=40)
            assert (run.returncode == 0) == successful, f'public {path}: {run.returncode}\n{run.stdout}\n{run.stderr}'
            if diagnostic:
                assert diagnostic in run.stderr, run.stderr
            assert 'partial-bootstrap-executed' not in run.stderr, run.stderr
            assert not list(download_tmp.iterdir()), 'download temporary files leaked'
            if successful:
                assert requests[-2:] == [f'/DanielNetzer/forgecell/releases/download/v{version}/{artifact}.sha256', f'/DanielNetzer/forgecell/releases/download/v{version}/{artifact}'], requests
            assert sentinel.read_text() == '{"preserve":true}'
            return run
        def install(version, successful=True):
            env['FORGECELL_VERSION'] = version
            env['FORGECELL_RELEASE_BASE'] = f'https://localhost:{server.server_port}'
            run = subprocess.run(['/bin/sh', str(release_root / version / 'install.sh')], env=env, text=True, capture_output=True, timeout=45)
            if (run.returncode == 0) != successful:
                raise AssertionError(f'install {version}: {run.returncode}\n{run.stdout}\n{run.stderr}')
            if successful:
                assert requests[-2:] == [f'/{version}/{artifact}.sha256', f'/{version}/{artifact}'], requests
            assert not list(download_tmp.iterdir()), 'download temporary files leaked'
            assert sentinel.read_text() == '{"preserve":true}'
            return run
        def current():
            return subprocess.check_output([str(bin_dir / 'forgecell'), '--version'], env=env, text=True).strip()
        for path, diagnostic in [('missing.sh', 'Forgecell: bootstrap download failed.'), ('empty.sh', 'Forgecell: bootstrap download was empty.'), ('stall.sh', 'Forgecell: bootstrap download failed.')]:
            public_install(versions[0], path, False, diagnostic)
            assert not bin_dir.exists(), 'failed bootstrap activated CLI'
            print(f'PASS: public one-liner rejects {path} before activation and cleans up')
        public_install(versions[0])
        assert current() == versions[0]
        print('PASS: exact public one-liner installs anonymously outside source checkout')
        for path, diagnostic in [('missing.sh', 'Forgecell: bootstrap download failed.'), ('empty.sh', 'Forgecell: bootstrap download was empty.')]:
            public_install(versions[0], path, False, diagnostic)
            assert current() == versions[0], 'bootstrap failure replaced active CLI'
        print('PASS: bootstrap failures preserve the active CLI')
        install(versions[0])
        assert current() == versions[0]
        print('PASS: verified HTTPS install and execution with no Node, Go, Git or gh on PATH')
        destination = served / versions[1] / artifact
        original = destination.read_bytes()
        destination.write_bytes(b'corrupted download')
        for installer in [public_install, install]:
            run = installer(versions[1], successful=False)
            assert 'checksum mismatch' in run.stderr
            assert current() == versions[0]
        print('PASS: corrupt upgrade rejected before activation')
        checksum_file = destination.with_name(artifact + '.sha256')
        original_checksum = checksum_file.read_bytes()
        for contents in [None, b'not-a-checksum\n', b'a' * 63 + b'\n']:
            if contents is None:
                checksum_file.unlink()
            else:
                checksum_file.write_bytes(contents)
            for installer in [public_install, install]:
                run = installer(versions[1], successful=False)
                if contents is not None:
                    assert 'invalid checksum' in run.stderr, run.stderr
                assert current() == versions[0]
        checksum_file.write_bytes(original_checksum)
        destination.write_bytes(original)
        for prefix, installer in [(f'/DanielNetzer/forgecell/releases/download/v{versions[1]}', public_install), (f'/{versions[1]}', install)]:
            server.partial_path = f'{prefix}/{artifact}'
            run = installer(versions[1], successful=False)
            assert 'curl:' in run.stderr, run.stderr
            assert current() == versions[0]
        server.partial_path = None
        print('PASS: missing/invalid checksums and partial binary downloads preserve active CLI')
        wrong_version = (served / versions[0] / artifact).read_bytes()
        destination.write_bytes(wrong_version)
        checksum_file.write_text(hashlib.sha256(wrong_version).hexdigest() + '\n')
        for installer in [public_install, install]:
            run = installer(versions[1], successful=False)
            assert 'release version mismatch' in run.stderr
            assert current() == versions[0]
        print('PASS: public one-liner and direct bootstrap reject checksum-valid version mismatch')
        destination.write_bytes(original)
        checksum_file.write_bytes(original_checksum)
        public_install(versions[1])
        assert current() == versions[1]
        install(versions[1])
        assert current() == versions[1]
        print('PASS: native upgrade and idempotent reinstall')
        rollback_env = {k:v for k,v in env.items() if k not in ['FORGECELL_HOME', 'FORGECELL_BIN']}
        subprocess.run([str(bin_dir / 'forgecell'), 'rollback'], env=rollback_env, check=True, capture_output=True, timeout=30)
        assert current() == versions[0]
        assert sentinel.read_text() == '{"preserve":true}'
        print('PASS: rollback and pre-existing Lab data preserved')
        installed = home / 'releases' / versions[0]
        manifest = json.loads((installed / 'manifest.json').read_text())
        assert hashlib.sha256((installed / 'forgecell').read_bytes()).hexdigest() == manifest['sha256']
        assert (installed / 'THIRD-PARTY-NOTICES.txt').stat().st_size > 1000
        print('PASS: installed manifest checksum and dependency notices')
    finally:
        stall_finished.set()
        server.shutdown()
        server.server_close()
