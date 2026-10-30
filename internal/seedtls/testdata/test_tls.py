import importlib.util
import json
import hashlib
import hmac
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
    def test_mount_order_is_not_drift_but_source_change_is(self):
        a = {'Config': {'Image': 'pinned'}, 'HostConfig': {'Binds': ['a:/a', 'b:/b']}, 'Mounts': [
            {'Source': '/a', 'Destination': '/a', 'RW': False},
            {'Source': '/b', 'Destination': '/b', 'RW': True}]}
        b = json.loads(json.dumps(a)); b['Mounts'].reverse()
        self.assertEqual(worker.container_fingerprint(a), worker.container_fingerprint(b))
        b['Mounts'][0]['Source'] = '/other'
        self.assertNotEqual(worker.container_fingerprint(a), worker.container_fingerprint(b))

    def test_rpc_salt_normalization_verifies_authentication(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)/'.dashmate'
            config = root/'mainnet/core/dash.conf'; config.parent.mkdir(parents=True)
            def profile(password):
                (root/'config.json').write_text(json.dumps({'configs': {'mainnet': {'core': {'rpc': {'users': {'dapi': {'password': password}}}}}}}))
            def render(salt, password, other='server=1'):
                digest = hmac.new(salt.encode(), password.encode(), hashlib.sha256).hexdigest()
                config.write_text('rpcauth=dapi:' + salt + '$' + digest + '\n' + other + '\n')
            profile('first-password')
            render('salt-one', 'first-password'); first = worker.configuration_digest(config)
            render('salt-two', 'first-password'); self.assertEqual(first, worker.configuration_digest(config))
            render('salt-two', 'first-password', 'server=0'); self.assertNotEqual(first, worker.configuration_digest(config))
            render('salt-two', 'wrong-password')
            with self.assertRaisesRegex(worker.Failure, 'rpc-auth-source-mismatch'):
                worker.configuration_digest(config)
            profile('changed-password'); render('salt-three', 'changed-password')
            self.assertNotEqual(first, worker.configuration_digest(config))

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
