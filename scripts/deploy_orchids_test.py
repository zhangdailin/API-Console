"""Exercise deployment and rollback with isolated files and stub service commands."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().with_name('deploy-orchids.sh')


class DeploymentTest(unittest.TestCase):
    def run_deploy(self, mode, metadata=True, manifest=True):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            helpers, install = root / 'bin', root / 'install'
            helpers.mkdir()
            install.mkdir()
            old, new = b'old binary fixture', b'new binary fixture'
            (install / 'orchids-server').write_bytes(old)
            old_metadata = {'build-info.txt': 'version=old\n',
                            'deploy-info.txt': 'sha256=' + hashlib.sha256(old).hexdigest() + '\n'}
            if metadata:
                for suffix, content in old_metadata.items():
                    (install / ('orchids-server.' + suffix)).write_text(content)
            artifact = root / 'artifact'
            artifact.write_bytes(new)
            checksum = root / 'artifact.sha256'
            checksum.write_text(hashlib.sha256(new).hexdigest() + '  artifact\n')
            build_info = root / 'artifact.build-info.txt'
            build_info.write_text('version=new\n')
            wrappers = {
                'file': '#!/bin/sh\necho "ELF 64-bit x86-64"\n',
                'sleep': '#!/bin/sh\nexit 0\n',
                'systemctl': '''#!/usr/bin/env python3
import os, pathlib, sys
p = pathlib.Path(__file__).parent / 'restarts'
if sys.argv[1] == 'restart':
 n = int(p.read_text()) + 1 if p.exists() else 1
 p.write_text(str(n))
 if os.environ['DEPLOY_TEST_MODE'] == 'restart-fails' and n == 1: sys.exit(1)
''',
                'curl': '''#!/usr/bin/env python3
import os, pathlib
p = pathlib.Path(__file__).parent / 'restarts'
n = int(p.read_text()) if p.exists() else 0
mode = os.environ['DEPLOY_TEST_MODE']
print('200' if mode == 'success' or (n > 1 and mode != 'rollback-fails') else '503', end='')
''',
                'date': '''#!/usr/bin/env python3
import pathlib, subprocess, sys
if sys.argv[1:] == ['+%s']:
 p = pathlib.Path(__file__).parent / 'clock'
 n = int(p.read_text()) + 1 if p.exists() else 1000
 p.write_text(str(n)); print(n)
else: subprocess.run(['/bin/date', *sys.argv[1:]], check=True)
'''}
            for name, content in wrappers.items():
                file = helpers / name
                file.write_text(content)
                file.chmod(0o700)
            args = ['bash', str(SCRIPT), '--artifact', str(artifact), '--checksum', str(checksum),
                    '--install-dir', str(install), '--service', 'test-fixture', '--health-path', '/health']
            if manifest:
                args += ['--build-info', str(build_info)]
            result = subprocess.run(args, env={**os.environ, 'PATH': str(helpers) + ':' + os.environ['PATH'],
                                               'HEALTH_TIMEOUT': '3', 'DEPLOY_TEST_MODE': mode},
                                    capture_output=True, text=True, timeout=15)
            expected_code = {'success': 0, 'rollback-fails': 7}.get(mode, 6)
            self.assertEqual(result.returncode, expected_code, result.stdout + result.stderr)
            self.assertEqual((install / 'orchids-server').read_bytes(), new if mode == 'success' else old)
            if mode == 'success':
                if manifest:
                    self.assertEqual((install / 'orchids-server.build-info.txt').read_text(), 'version=new\n')
                else:
                    self.assertFalse((install / 'orchids-server.build-info.txt').exists())
                self.assertIn(hashlib.sha256(new).hexdigest(), (install / 'orchids-server.deploy-info.txt').read_text())
            else:
                for suffix, content in old_metadata.items():
                    target = install / ('orchids-server.' + suffix)
                    if metadata:
                        self.assertEqual(target.read_text(), content)
                    else:
                        self.assertFalse(target.exists())
                self.assertIn('rollback_status=' + ('failed' if mode == 'rollback-fails' else 'healthy'),
                              (install / 'orchids-server.last-failed-deploy.txt').read_text())

    def test_success(self):
        self.run_deploy('success')

    def test_manual_build_removes_stale_metadata(self):
        self.run_deploy('success', manifest=False)

    def test_health_failure_restores_metadata(self):
        self.run_deploy('unhealthy')

    def test_restart_failure_rolls_back(self):
        self.run_deploy('restart-fails')

    def test_absent_old_metadata_stays_absent(self):
        self.run_deploy('unhealthy', metadata=False)

    def test_failed_rollback_is_reported(self):
        self.run_deploy('rollback-fails')


if __name__ == '__main__':
    unittest.main()
