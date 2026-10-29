import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("production_deploy", Path(__file__).with_name("deploy.py"))
deploy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deploy)


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.sha = "a" * 40
        self.manifest = {"commit": self.sha, "image_ref": "aistudio-api:sha-" + self.sha,
                         "version": "main-" + self.sha[:12], "image_id": "sha256:" + "d" * 64,
                         "local_image_id": "sha256:" + "e" * 64}
        image = self.root / "aistudio-api-image.tar.gz"
        image.write_bytes(b"image-fixture")
        self.manifest["artifact_checksum"] = hashlib.sha256(image.read_bytes()).hexdigest()
        (self.root / "release.json").write_text(json.dumps(self.manifest))
        self.sums = "\n".join(deploy.digest(self.root / name) + "  " + name for name in [image.name, "release.json"])
        (self.root / "SHA256SUMS").write_text(self.sums)

    def test_release_is_bound_to_full_commit_and_both_checksums(self):
        self.assertEqual(deploy.verify_bundle(self.root, self.sha)["commit"], self.sha)
        with self.assertRaisesRegex(RuntimeError, "selected main"):
            deploy.verify_bundle(self.root, "b" * 40)
        (self.root / "aistudio-api-image.tar.gz").write_bytes(b"truncated")
        with self.assertRaisesRegex(RuntimeError, "checksum mismatch"):
            deploy.verify_bundle(self.root, self.sha)

    def test_checksum_paths_cannot_escape_release_directory(self):
        (self.root / "SHA256SUMS").write_text("0" * 64 + "  ../../runtime.env\n")
        with self.assertRaisesRegex(RuntimeError, "unexpected"):
            deploy.verify_bundle(self.root, self.sha)

    def test_stale_commit_does_not_touch_container(self):
        with patch.object(deploy, "current_main", return_value="b" * 40), patch.object(deploy, "run") as run:
            deploy.deploy(self.root, self.manifest, self.sha)
        run.assert_not_called()

    def _check_failed_docker_creation(self, health):
        compose = self.root / "docker-compose.yml"
        compose.write_text("original compose")
        (self.root / "runtime.env").write_text("TZ=UTC\n")
        marker = self.root / "maintenance"
        old = {"Id": "c" * 64, "Image": "old-image", "State": {"Running": True, "Pid": 123}, "Config": {"Env": []}}
        calls = []

        def command(args, **kwargs):
            calls.append(args)
            if args[:2] == ["docker", "inspect"]:
                return json.dumps([old])
            if args[:2] == ["docker", "run"]:
                raise RuntimeError("container creation failed after reserving its name")
            if args[:2] == ["docker", "rm"]:
                return "removed failed candidate"
            return "ok"

        with patch.object(deploy, "ROOT", self.root), patch.object(deploy, "COMPOSE", compose), \
             patch.object(deploy, "MAINTENANCE", marker), patch.object(deploy, "current_main", return_value=self.sha), \
             patch.object(deploy, "active_images", return_value=0), patch.object(deploy, "active_http_connections", return_value=0), \
             patch.object(deploy, "healthy", return_value=health), \
             patch.object(deploy.time, "sleep"), patch.object(deploy, "run", side_effect=command):
            with self.assertRaisesRegex(RuntimeError, "container creation failed"):
                deploy.deploy(self.root, self.manifest, self.sha)
        self.assertIn(["docker", "rm", "--force", "new-api"], calls)
        self.assertIn(["docker", "rename", "new-api-previous-" + "c" * 12, "new-api"], calls)
        self.assertIn(["docker", "start", "new-api"], calls)
        self.assertIn(["docker", "update", "--restart=no", "new-api"], calls)
        self.assertIn(["docker", "update", "--restart=always", "new-api"], calls)
        self.assertEqual(compose.read_text(), "original compose")
        self.assertEqual(marker.exists(), not health)
        self.assertFalse((self.root / "deployed.json").exists())

    def test_failed_docker_creation_restores_previous_container_and_config(self):
        self._check_failed_docker_creation(True)

    def test_unhealthy_rollback_keeps_maintenance(self):
        self._check_failed_docker_creation(False)

    def test_stale_state_cannot_hide_manual_runtime_rollback(self):
        state = {"commit": self.sha, "version": self.manifest["version"], "image_id": "expected-image"}
        (self.root / "deployed.json").write_text(json.dumps(state))
        for image, should_download in [("expected-image", False), ("rolled-back-image", True)]:
            with self.subTest(image=image):
                running = {"State": {"Running": True}, "Image": image,
                           "Config": {"Labels": {"org.opencontainers.image.revision": self.sha}}}
                with patch.object(deploy, "ROOT", self.root), patch.object(deploy, "current_main", return_value=self.sha), \
                     patch.object(deploy, "healthy", return_value=True), patch.object(deploy, "run", return_value=json.dumps([running])), \
                     patch.object(deploy, "download", side_effect=RuntimeError("release required")) as download:
                    if should_download:
                        with self.assertRaisesRegex(RuntimeError, "release required"):
                            deploy.poll()
                    else:
                        deploy.poll()
                        download.assert_not_called()


if __name__ == "__main__":
    unittest.main()
