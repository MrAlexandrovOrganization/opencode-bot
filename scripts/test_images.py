"""Synthetic delivery checks; never contact a registry or start a service."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location(
    "images", Path(__file__).with_name("images.py")
)
images = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(images)


class DeliveryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.manifest = {
            "revision": "a" * 40,
            "platform": "linux/amd64",
            "images": {"app": "ghcr.io/example/project/app@sha256:" + "b" * 64},
        }
        self.addCleanup(patch.stopall)
        patch.object(images, "ROOT", self.root).start()
        patch.object(images, "STATE", self.root / ".images").start()
        patch.object(images, "revision", return_value="a" * 40).start()
        patch.object(images, "specs", return_value={"app": {"context": "."}}).start()
        patch.dict(
            os.environ, {"IMAGE_COMPOSE_FILE": "compose.yaml"}, clear=True
        ).start()

    def test_operate_uses_active_image_after_configuration_commit(self):
        images.STATE.mkdir()
        (images.STATE / "current.json").write_text(json.dumps(self.manifest))
        with (
            patch.object(images, "revision", return_value="c" * 40),
            patch.object(images, "compose", return_value=["docker", "compose"]),
            patch.object(images, "run") as run,
        ):
            images.operate()
        self.assertIn(str(images.STATE / "compose.json"), run.call_args.args)
        self.assertEqual(run.call_args.args[-1], "ps")

    def save_manifest(self):
        path = self.root / "manifest.json"
        path.write_text(json.dumps(self.manifest))
        os.environ["IMAGE_MANIFEST"] = str(path)

    def test_rejects_other_revision_and_mutable_tag(self):
        self.manifest["revision"] = "c" * 40
        with self.assertRaises(ValueError):
            images.validate(self.manifest)
        self.manifest["revision"] = "a" * 40
        self.manifest["images"]["app"] = "ghcr.io/example/project/app:latest"
        with self.assertRaises(ValueError):
            images.validate(self.manifest)

    def test_rejects_missing_services(self):
        self.manifest["images"] = {}
        with self.assertRaises(ValueError):
            images.validate(self.manifest)

    def test_pull_failure_never_activates_or_overwrites_current(self):
        self.save_manifest()
        current = images.STATE / "current.json"
        images.write(current, {"old": True})
        with patch.object(
            images, "run", side_effect=subprocess.CalledProcessError(1, "pull")
        ) as run:
            with self.assertRaises(subprocess.CalledProcessError):
                images.deploy()
        self.assertEqual(json.loads(current.read_text()), {"old": True})
        self.assertEqual(run.call_count, 1)

    def test_activation_failure_keeps_current(self):
        self.save_manifest()
        current = images.STATE / "current.json"
        images.write(current, {"old": True})

        def fail_activation(*args, **kwargs):
            if args[0] == "make":
                raise subprocess.CalledProcessError(1, "make")

        with patch.object(images, "run", side_effect=fail_activation):
            with self.assertRaises(subprocess.CalledProcessError):
                images.deploy()
        self.assertEqual(json.loads(current.read_text()), {"old": True})

    def test_success_preserves_override_and_previous_manifest(self):
        self.save_manifest()
        images.write(images.STATE / "current.json", {"old": True})
        (self.root / "docker-compose.override.yml").write_text("services: {}")
        with patch.object(images, "run") as run:
            images.deploy()
        calls = [call.args for call in run.call_args_list]
        self.assertEqual(calls[0][:2], ("docker", "pull"))
        self.assertEqual(calls[-1][:2], ("make", "image-activate"))
        self.assertIn("docker-compose.override.yml", calls[-1][2])
        self.assertEqual(
            json.loads((images.STATE / "current.json").read_text()), self.manifest
        )
        self.assertEqual(
            json.loads((images.STATE / "previous.json").read_text()), {"old": True}
        )

    def test_build_deduplicates_migration_image_and_uses_exported_versions(self):
        os.environ["GO_VERSION"] = "synthetic-version"
        spec = {"context": ".", "args": {"GO_VERSION": "${GO_VERSION:?Run via make}"}}
        with patch.object(images, "specs", return_value={"app": spec, "migrate": spec}):
            with patch.object(images, "run") as run:
                images.build()
        self.assertEqual(run.call_count, 1)
        self.assertIn("GO_VERSION=synthetic-version", run.call_args.args)
        manifest = json.loads((images.STATE / "build.json").read_text())
        self.assertEqual(manifest["images"]["app"], manifest["images"]["migrate"])

    def test_publish_pushes_existing_images_and_records_digest_without_building(self):
        manifest = dict(self.manifest)
        manifest["images"] = {"app": "ghcr.io/example/project/app:checked"}
        images.write(images.STATE / "build.json", manifest)
        os.environ["GITHUB_OUTPUT"] = str(self.root / "output")

        def docker(*args, **kwargs):
            if args[:3] == ("docker", "image", "inspect"):
                return json.dumps([self.manifest["images"]["app"]])

        with patch.object(images, "run", side_effect=docker) as run:
            images.publish()
        self.assertEqual(run.call_count, 2)
        self.assertEqual(run.call_args_list[0].args[:2], ("docker", "push"))
        self.assertEqual(
            json.loads((images.STATE / "release.json").read_text()), self.manifest
        )
        self.assertTrue((self.root / "output").read_text().startswith("manifest="))

    def test_explicit_migration_uses_active_release_without_build_or_pull(self):
        images.write(images.STATE / "current.json", self.manifest)
        os.environ["IMAGE_ARGS"] = "run --rm app migrate"
        with patch.object(images, "run") as run:
            images.operate()
        self.assertIn(str(images.STATE / "compose.json"), run.call_args.args)
        self.assertIn("--no-build", run.call_args.args)
        self.assertIn("never", run.call_args.args)

    def test_alias_is_required_in_manifest(self):
        os.environ["IMAGE_ALIASES"] = "migrate=app"
        with self.assertRaises(ValueError):
            images.validate(self.manifest)
        self.manifest["images"]["migrate"] = self.manifest["images"]["app"]
        images.validate(self.manifest)
        self.manifest["images"]["migrate"] = (
            "ghcr.io/example/project/app@sha256:" + "c" * 64
        )
        with self.assertRaises(ValueError):
            images.validate(self.manifest)

    def test_manual_deploy_holds_the_same_lock_as_cd(self):
        os.environ["IMAGE_DEPLOY_LOCK"] = str(self.root / "deploy.lock")

        def assert_locked():
            with (self.root / "deploy.lock").open("a") as lock:
                with self.assertRaises(BlockingIOError):
                    images.fcntl.flock(
                        lock, images.fcntl.LOCK_EX | images.fcntl.LOCK_NB
                    )

        with patch.object(images, "deploy", side_effect=assert_locked):
            images.locked_deploy()

    def test_cd_reuses_its_existing_checkout_lock(self):
        os.environ["IMAGE_DEPLOY_LOCKED"] = "1"
        with patch.object(images, "deploy") as deploy:
            images.locked_deploy()
        deploy.assert_called_once()


if __name__ == "__main__":
    unittest.main()
