"""Negative and data-path tests for the effective Docker recipe converter."""
import importlib.util
from pathlib import Path
import unittest
import tempfile
import hashlib
import hmac
import json

spec=importlib.util.spec_from_file_location('managed_worker',Path(__file__).parents[1]/'worker.py')
worker=importlib.util.module_from_spec(spec);spec.loader.exec_module(worker)


class RecipeTest(unittest.TestCase):
    def test_generated_rpcauth_requires_same_verified_credentials(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)/'.dashmate'
            path=root/'testnet/core/dash.conf';path.parent.mkdir(parents=True)
            source=root/'config.json'
            w=worker.Worker(dict(fleet=dict(metadata=dict(name='fixture')),target={}))
            def generate(salt, password='test-only', setting='server=1'):
                source.write_text(json.dumps({'configs':{'testnet':{'core':{'rpc':{'users':{'fixture':{'password':password}}}}}}}))
                digest=hmac.new(salt.encode(),password.encode(),hashlib.sha256).hexdigest()
                path.write_text(setting+'\nrpcauth=fixture:'+salt+'$'+digest+'\n')
                return w.configuration_digest(path)
            baseline=generate('a'*32)
            self.assertEqual(baseline,generate('b'*32))
            self.assertNotEqual(baseline,generate('c'*32,password='different-secret'))
            self.assertNotEqual(baseline,generate('d'*32,setting='server=0'))
            generate('e'*32)
            source.write_text(json.dumps({'configs':{'testnet':{'core':{'rpc':{'users':{'fixture':{'password':'wrong'}}}}}}}))
            with self.assertRaises(worker.Failure):w.configuration_digest(path)

    def test_unrecognized_core_config_retains_exact_byte_hash(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'dash.conf';path.write_text('rpcauth=opaque\n')
            w=worker.Worker(dict(fleet=dict(metadata=dict(name='fixture')),target={}))
            self.assertEqual(w.configuration_digest(path),hashlib.sha256(path.read_bytes()).hexdigest())

    def test_existing_volume_subpath_and_tmpfs_survive(self):
        container=dict(Id='a'*64,Config={},NetworkSettings=dict(Networks={}),
            HostConfig=dict(Mounts=[
                dict(Type='volume',Source='existing',Target='/data',VolumeOptions=dict(Subpath='db')),
                dict(Type='tmpfs',Target='/memory',TmpfsOptions=dict(SizeBytes=1048576,Mode=448))]),
            Mounts=[dict(Type='volume',Name='existing',Source='/var/lib/docker/volumes/existing/_data/db',Destination='/data',RW=True),
                    dict(Type='tmpfs',Destination='/memory',RW=True)])
        w=worker.Worker(dict(fleet=dict(metadata=dict(name='fixture')),target={}))
        recipe=w.recipe(container)
        mounts={m['Target']:m for m in recipe['HostConfig']['Mounts']}
        self.assertEqual(mounts['/data']['Source'],'existing')
        self.assertEqual(mounts['/data']['VolumeOptions'],dict(Subpath='db',NoCopy=True))
        self.assertEqual(mounts['/memory']['TmpfsOptions'],dict(SizeBytes=1048576,Mode=448))
        # Conversion must not mutate the original inspected object either.
        self.assertNotIn('NoCopy',container['HostConfig']['Mounts'][0]['VolumeOptions'])

    def test_legacy_selinux_flags_and_anonymous_volume_are_preserved(self):
        container=dict(Id='a'*64,Config={},NetworkSettings=dict(Networks={}),HostConfig={},
                       Mounts=[dict(Type='bind',Source='/config',Destination='/config',RW=False,Mode='ro,Z'),
                               dict(Type='volume',Name='actual-anonymous',Destination='/data',RW=True,Mode='')])
        w=worker.Worker(dict(fleet=dict(metadata=dict(name='fixture')),target={}))
        self.assertEqual(w.recipe(container)['HostConfig']['Binds'],
                         ['/config:/config:Z,ro','actual-anonymous:/data:nocopy,rw'])


if __name__=='__main__':unittest.main()
