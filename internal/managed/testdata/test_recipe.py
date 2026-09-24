"""Negative and data-path tests for the effective Docker recipe converter."""
import importlib.util
from pathlib import Path
import unittest

spec=importlib.util.spec_from_file_location('managed_worker',Path(__file__).parents[1]/'worker.py')
worker=importlib.util.module_from_spec(spec);spec.loader.exec_module(worker)


class RecipeTest(unittest.TestCase):
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
