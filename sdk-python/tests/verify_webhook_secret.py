"""Exercise webhook verification without loading the HTTP client dependencies."""

import importlib
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

sdk = Path(sys.argv.pop(1)).resolve()
for name, path in (("impreza", sdk / "impreza"), ("impreza.models", sdk / "impreza" / "models")):
    package = types.ModuleType(name)
    package.__path__ = [str(path)]
    sys.modules[name] = package

webhooks = importlib.import_module("impreza.webhooks")


class WebhookCredentialTests(unittest.TestCase):
    def test_missing_secret_stops_before_signature_computation(self):
        boundary = Mock(side_effect=AssertionError("verification reached signature computation"))
        with (
            patch.object(webhooks, "compute_signature", boundary),
            self.assertRaises(webhooks.WebhookSignatureMismatch),
        ):
            webhooks.verify_signature(body=b"{}", signature_header="sha256=unused", secret="")
        boundary.assert_not_called()

    def test_subscription_secret_authenticates_event(self):
        body = b'{"id":"evt_fixture","type":"fixture.checked","created_at":"2026-01-01T00:00:00Z"}'
        secret = "synthetic-subscription-secret"
        event = webhooks.verify_signature(
            body=body, signature_header=webhooks.compute_signature(body, secret), secret=secret
        )
        self.assertEqual(event.id, "evt_fixture")


if __name__ == "__main__":
    unittest.main()
