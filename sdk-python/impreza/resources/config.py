"""Config-as-code for custom deployments — ``Client.config`` and
``AsyncClient.config`` (C1 / P4).

Wraps the reviewed-configuration surface the platform exposes for one
custom deployment:

* ``GET  /platform/deployments/custom/{id}/config``        — export the
  versioned document (secrets are references only, never values);
* ``POST /platform/deployments/custom/{id}/prepare-config-apply`` — build a
  reviewed apply from a document, returning the plan id, the review digest
  and the server-computed change summary / diff text;
* ``GET  /platform/config-plans/{id}``                      — read a saved
  review or its receipt;
* ``POST /platform/config-plans/{id}/apply``                — apply the
  reviewed plan. Requires the exact ``review_digest`` and an explicit
  confirmation; replays of an accepted plan return the same receipt.

The document itself is a JSON string the caller keeps under version
control. This resource deliberately returns plain dictionaries: the
document's shape is schema-versioned by the server and intentionally
round-trips unchanged, so hand-modelled classes here would only drift
from the server's schema.
"""

from __future__ import annotations

import re
from typing import TYPE_CHECKING, Any

from ..exceptions import ApiError

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


def _data(payload: object) -> dict[str, Any]:
    body = payload.get("data") if isinstance(payload, dict) else None
    if not isinstance(body, dict):
        raise ApiError("The config response has no data object.", code="INVALID_RESPONSE")
    return body


def _identifier(value: str, kind: str) -> str:
    pattern = (
        r"dpl_(?:[a-f0-9]{16}|[a-f0-9]{24})" if kind == "deployment" else r"cplan_[a-f0-9]{24}"
    )
    if not isinstance(value, str) or re.fullmatch(pattern, value) is None:
        raise ValueError(f"Invalid {kind} identifier.")
    return value


def _digest(value: str) -> str:
    if not isinstance(value, str) or re.fullmatch(r"[a-f0-9]{64}", value) is None:
        raise ValueError("Invalid config review digest.")
    return value


def _document(value: str) -> str:
    if not isinstance(value, str) or not value or len(value.encode("utf-8")) > 65536:
        raise ValueError("Config document must contain 1 to 65536 UTF-8 bytes.")
    return value


class ConfigResource:
    """Sync access to config-as-code for custom deployments."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def export(self, deployment_id: str) -> dict[str, Any]:
        """Export the versioned config document of one deployment.

        The response carries the parsed ``document`` and a pretty-printed
        ``document_text`` suitable for writing straight to a file. Secrets
        appear as references only — applying the document resolves them
        server-side.
        """
        return _data(
            self._http.get(
                f"/platform/deployments/custom/{_identifier(deployment_id, 'deployment')}/config"
            )
        )

    def prepare(self, deployment_id: str, document: str) -> dict[str, Any]:
        """Build a reviewed apply from a config document.

        ``document`` is the exact JSON text (typically the file written by
        :meth:`export`). The response includes ``plan_id``,
        ``review_digest`` and the server-computed ``changes`` /
        ``diff_text`` summary the caller must review before applying.
        """
        return _data(
            self._http.post(
                f"/platform/deployments/custom/{_identifier(deployment_id, 'deployment')}"
                "/prepare-config-apply",
                json={"document": _document(document)},
            )
        )

    def get_plan(self, plan_id: str) -> dict[str, Any]:
        """Read one saved config review (status, digest, receipt)."""
        return _data(self._http.get(f"/platform/config-plans/{_identifier(plan_id, 'plan')}"))

    def apply(self, plan_id: str, review_digest: str) -> dict[str, Any]:
        """Apply a reviewed config plan.

        Both the plan id and the exact ``review_digest`` from the prepare
        response are required; the server re-verifies ownership, expiry and
        the deployment's configuration before anything is queued.
        """
        return _data(
            self._http.post(
                f"/platform/config-plans/{_identifier(plan_id, 'plan')}/apply",
                json={"confirm": True, "review_digest": _digest(review_digest)},
            )
        )


class AsyncConfigResource:
    """Async counterpart to :class:`ConfigResource`."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def export(self, deployment_id: str) -> dict[str, Any]:
        """See :meth:`ConfigResource.export`."""
        return _data(
            await self._http.get(
                f"/platform/deployments/custom/{_identifier(deployment_id, 'deployment')}/config"
            )
        )

    async def prepare(self, deployment_id: str, document: str) -> dict[str, Any]:
        """See :meth:`ConfigResource.prepare`."""
        return _data(
            await self._http.post(
                f"/platform/deployments/custom/{_identifier(deployment_id, 'deployment')}"
                "/prepare-config-apply",
                json={"document": _document(document)},
            )
        )

    async def get_plan(self, plan_id: str) -> dict[str, Any]:
        """See :meth:`ConfigResource.get_plan`."""
        return _data(await self._http.get(f"/platform/config-plans/{_identifier(plan_id, 'plan')}"))

    async def apply(self, plan_id: str, review_digest: str) -> dict[str, Any]:
        """See :meth:`ConfigResource.apply`."""
        return _data(
            await self._http.post(
                f"/platform/config-plans/{_identifier(plan_id, 'plan')}/apply",
                json={"confirm": True, "review_digest": _digest(review_digest)},
            )
        )
