import importlib.util
import json
from pathlib import Path
import subprocess
import socket
import ssl
import tempfile
import threading
import unittest
from unittest import mock


def load(name):
    p = Path(__file__).resolve().parents[1] / (name + '.py')
    spec = importlib.util.spec_from_file_location('seed_' + name, p)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


renew = load('renew')
worker = load('worker')


class CertificateTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.tmp.name)
        def openssl(*args):
            subprocess.run(['openssl', *args], cwd=cls.root, check=True, capture_output=True)
        openssl('req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=Test CA', '-days', '10', '-keyout', 'ca.key', '-out', 'ca.crt')
        openssl('req', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=seed.example.org', '-keyout', 'leaf.key', '-out', 'leaf.csr')
        (cls.root/'san').write_text('subjectAltName=DNS:seed.example.org,IP:8.8.8.8\nextendedKeyUsage=serverAuth\n')
        openssl('x509', '-req', '-in', 'leaf.csr', '-CA', 'ca.crt', '-CAkey', 'ca.key', '-CAcreateserial', '-days', '5', '-extfile', 'san', '-out', 'leaf.crt')
        (cls.root/'trust').mkdir()
        (cls.root/'trust/ca.crt').write_bytes((cls.root/'ca.crt').read_bytes())
        openssl('rehash', str(cls.root/'trust'))

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def test_real_chain_hostname_ip_and_key(self):
        cfg = {'hostname': 'seed.example.org', 'address': '8.8.8.8'}
        renew.verify_pair(self.root/'leaf.crt', self.root/'leaf.key', cfg, str(self.root/'trust'))
        for changed in [dict(cfg, hostname='other.example.org'), dict(cfg, address='8.8.4.4')]:
            with self.assertRaises(Exception):
                renew.verify_pair(self.root/'leaf.crt', self.root/'leaf.key', changed, str(self.root/'trust'))
        with self.assertRaises(renew.Failure):
            renew.verify_pair(self.root/'leaf.crt', self.root/'ca.key', cfg, str(self.root/'trust'))
        with self.assertRaises(renew.Failure):
            renew.verify_pair(self.root/'leaf.crt', self.root/'leaf.key', cfg)

    def test_real_served_tls_validates_both_identities(self):
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(str(self.root/'leaf.crt'), str(self.root/'leaf.key'))
        listener = socket.socket()
        listener.bind(('127.0.0.1', 0))
        listener.listen(4)
        listener.settimeout(5)
        address = listener.getsockname()
        errors = []
        def server():
            try:
                for _ in range(2):
                    client, _ = listener.accept()
                    with context.wrap_socket(client, server_side=True) as tls:
                        tls.recv(1)
            except Exception as e:
                errors.append(e)
            finally:
                listener.close()
        thread = threading.Thread(target=server, daemon=True)
        thread.start()
        real_connect = socket.create_connection
        real_context = ssl.create_default_context
        with mock.patch.object(renew.socket, 'create_connection', side_effect=lambda *_a, **_kw: real_connect(address, timeout=5)), mock.patch.object(renew.ssl, 'create_default_context', side_effect=lambda: real_context(cafile=str(self.root/'ca.crt'))):
            cfg = {'hostname': 'seed.example.org', 'address': '8.8.8.8'}
            renew.wait_peer(cfg, renew.leaf_der((self.root/'leaf.crt').read_bytes()))
        thread.join(6)
        self.assertFalse(thread.is_alive())
        self.assertEqual(errors, [])


class InstallationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        for name, value in [('served.crt', b'old-cert'), ('served.key', b'old-key'), ('new.crt', b'new-cert'), ('new.key', b'new-key')]:
            (self.root/name).write_bytes(value)
        self.cfg = {'certificate': str(self.root/'served.crt'), 'key': str(self.root/'served.key'), 'gateway': 'gateway', 'hostname': 'seed.example.org', 'address': '8.8.8.8'}

    def test_success_preserves_bind_mount_inodes(self):
        inodes = [Path(self.cfg[x]).stat().st_ino for x in ['certificate', 'key']]
        with mock.patch.object(renew, 'verify_pair'), mock.patch.object(renew, 'reload') as reload, mock.patch.object(renew, 'wait_peer'), mock.patch.object(renew, 'leaf_der', side_effect=lambda x: x):
            renew.install(self.root, self.cfg, self.root/'new.crt', self.root/'new.key')
        self.assertEqual(inodes, [Path(self.cfg[x]).stat().st_ino for x in ['certificate', 'key']])
        self.assertEqual((self.root/'served.crt').read_bytes(), b'new-cert')
        self.assertFalse((self.root/'pending-install.json').exists())
        reload.assert_called_once()

    def test_failed_reload_restores_both_files(self):
        with mock.patch.object(renew, 'verify_pair'), mock.patch.object(renew, 'reload', side_effect=[renew.Failure('reload-failed'), None]), mock.patch.object(renew, 'wait_peer'), mock.patch.object(renew, 'leaf_der', side_effect=lambda x: x):
            with self.assertRaises(renew.Failure):
                renew.install(self.root, self.cfg, self.root/'new.crt', self.root/'new.key')
        self.assertEqual((self.root/'served.crt').read_bytes(), b'old-cert')
        self.assertEqual((self.root/'served.key').read_bytes(), b'old-key')

    def test_interrupted_pair_recovered_before_reissue(self):
        original = renew.write_existing
        def power_loss(path, data):
            if path == self.cfg['certificate']:
                raise KeyboardInterrupt()
            original(path, data)
        with mock.patch.object(renew, 'verify_pair'), mock.patch.object(renew, 'write_existing', side_effect=power_loss):
            with self.assertRaises(KeyboardInterrupt):
                renew.install(self.root, self.cfg, self.root/'new.crt', self.root/'new.key')
        self.assertTrue((self.root/'pending-install.json').exists())
        self.assertEqual((self.root/'served.key').read_bytes(), b'new-key')
        with mock.patch.object(renew, 'reload'), mock.patch.object(renew, 'wait_peer'), mock.patch.object(renew, 'leaf_der', side_effect=lambda x: x):
            renew.recover(self.root, self.cfg)
        self.assertEqual((self.root/'served.key').read_bytes(), b'old-key')
        self.assertEqual((self.root/'served.crt').read_bytes(), b'old-cert')

    def test_invalid_certificate_never_writes(self):
        with mock.patch.object(renew, 'verify_pair', side_effect=renew.Failure('invalid')), mock.patch.object(renew, 'write_existing') as write:
            with self.assertRaises(renew.Failure):
                renew.install(self.root, self.cfg, self.root/'new.crt', self.root/'new.key')
            write.assert_not_called()

    def test_recovery_rejects_tampered_backup(self):
        (self.root/'pending-install.json').write_text(json.dumps({'certificate': '/wrong', 'key': self.cfg['key']}))
        with self.assertRaisesRegex(renew.Failure, 'recovery-path-mismatch'):
            renew.recover(self.root, self.cfg)

    def test_symlink_not_followed(self):
        link = self.root/'link'; link.symlink_to(self.root/'served.key')
        with self.assertRaises(OSError):
            renew.write_existing(link, b'oops')
        self.assertEqual((self.root/'served.key').read_bytes(), b'old-key')


class PreservationTests(unittest.TestCase):
    def test_failed_adoption_restores_bounded_helper_restart_policy(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            systemd = base / 'systemd'; systemd.mkdir()
            certificate = base / 'served.crt'; certificate.write_bytes(b'original-cert')
            key = base / 'served.key'; key.write_bytes(b'original-key')
            root = base / 'operation'
            target = {'name': 'seed-1', 'helper': 'existing-helper',
                      'hostname': 'seed.example.org', 'address': '8.8.8.8'}
            baseline = {'paths': {'certificate': str(certificate), 'key': str(key)}}
            helper = {'Name': '/existing-helper', 'State': {'Running': True},
                      'HostConfig': {'RestartPolicy': {'Name': 'on-failure', 'MaximumRetryCount': 5}}}
            plan = {'id': 'test-plan', 'snapshot': {'target': target, 'baseline': baseline,
                    'fingerprint': worker.sha(baseline)}, 'acmeImage': 'example@sha256:test',
                    'email': 'ops@example.org'}
            renewal = '''
def issue(*args, **kwargs):
    raise ValueError('simulated-issuance-failure')
def recover(*args): pass
def write_existing(*args): pass
def reload(*args): pass
'''
            real_require = worker.require
            def require(condition, message):
                # The test operation directory is task-owned, not root-owned.
                if message != 'unsafe-operation-directory':
                    real_require(condition, message)
            def path(value):
                return systemd if value == '/etc/systemd/system' else Path(value)
            with mock.patch.object(worker, 'require', side_effect=require), \
                    mock.patch.object(worker, 'Path', side_effect=path), \
                    mock.patch.object(worker, 'observe', return_value=(baseline, [helper])), \
                    mock.patch.object(worker, 'core_ready'), \
                    mock.patch.object(worker, 'check_retirement'), \
                    mock.patch.object(worker, 'backup_package'), \
                    mock.patch.object(worker.socket, 'getaddrinfo', return_value=[(0, 0, 0, '', ('8.8.8.8', 443))]), \
                    mock.patch.object(worker.socket, 'socket'), \
                    mock.patch.object(worker, 'run') as run:
                with self.assertRaisesRegex(ValueError, 'simulated-issuance-failure'):
                    worker.apply({'target': target, 'plan': plan, 'renewSource': renewal}, root)
            updates = [call.args[0] for call in run.call_args_list
                       if call.args[0][:2] == ['docker', 'update']]
            self.assertEqual(updates, [
                ['docker', 'update', '--restart=no', 'existing-helper'],
                ['docker', 'update', '--restart=on-failure:5', 'existing-helper'],
            ])
            run.assert_any_call(['docker', 'start', 'existing-helper'])
            self.assertEqual(json.loads((root / 'operation.json').read_text())['phase'], 'rolled-back')
            self.assertEqual(certificate.read_bytes(), b'original-cert')
            self.assertEqual(key.read_bytes(), b'original-key')

    def test_container_configuration_and_start_time_preserved(self):
        expected = {'containers': {'core': {'id': 'a', 'startedAt': 'before'}, 'helper': {'id': 'h'}}, 'files': {'/config': 'sha'}}
        current = {'containers': {'core': {'id': 'a', 'startedAt': 'before'}}, 'files': {'/config': 'sha'}}
        worker.assert_preserved(expected, current, 'helper')
        current['containers']['core']['startedAt'] = 'after'
        with self.assertRaisesRegex(worker.Failure, 'preserved-container-drift'):
            worker.assert_preserved(expected, current, 'helper')

    def test_drifted_config_rejected(self):
        expected = {'containers': {}, 'files': {'/config': 'old'}}
        with self.assertRaisesRegex(worker.Failure, 'preserved-configuration-drift'):
            worker.assert_preserved(expected, {'containers': {}, 'files': {'/config': 'new'}}, 'helper')


if __name__ == '__main__':
    unittest.main()
