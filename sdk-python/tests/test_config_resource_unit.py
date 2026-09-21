"""Unit tests for the config-as-code SDK resource (C1 / P4).

Covers ``c.config`` and ``ac.config`` over respx: export parses the
envelope's data payload, prepare posts the exact document, get_plan reads
a review, and apply posts the digest + confirmation pair verbatim. Also
pins the failure mapping: a 409 digest mismatch surfaces as ApiError with
the upstream code attached.
"""

from __future__ import annotations

import json

import httpx
import pytest
import respx

from impreza import ApiError, AsyncClient, Client

BASE = "https://api.imprezahost.com/v1"

_DEPLOYMENT = "dpl_" + "c" * 16
_PLAN = "cplan_" + "6" * 24
_DIGEST = "f" * 64
_DOC_TEXT = json.dumps({"schema": 1, "deployment": {"name": "web"}, "resources": {}})


def _envelope(data: dict[str, object]) -> dict[str, object]:
    return {"success": True, "data": data, "meta": {"request_id": "req_t"}}


@respx.mock
def test_export_returns_data_payload() -> None:
    respx.get(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/config").mock(
        return_value=httpx.Response(
            200, json=_envelope({"document_text": _DOC_TEXT + "\n", "schema": 1})
        )
    )
    with Client(api_key="k", api_secret="s") as c:
        out = c.config.export(_DEPLOYMENT)
    assert out["document_text"] == _DOC_TEXT + "\n"
    assert out["schema"] == 1


@respx.mock
def test_prepare_posts_exact_document() -> None:
    route = respx.post(
        f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/prepare-config-apply"
    ).mock(
        return_value=httpx.Response(
            201, json=_envelope({"plan_id": _PLAN, "review_digest": _DIGEST})
        )
    )
    with Client(api_key="k", api_secret="s") as c:
        out = c.config.prepare(_DEPLOYMENT, _DOC_TEXT)
    assert out["plan_id"] == _PLAN
    body = json.loads(route.calls.last.request.content)
    assert body == {"document": _DOC_TEXT}


@respx.mock
def test_apply_posts_digest_and_confirmation() -> None:
    route = respx.post(f"{BASE}/platform/config-plans/{_PLAN}/apply").mock(
        return_value=httpx.Response(202, json=_envelope({"command_id": "cmd_x"}))
    )
    with Client(api_key="k", api_secret="s") as c:
        out = c.config.apply(_PLAN, _DIGEST)
    assert out["command_id"] == "cmd_x"
    body = json.loads(route.calls.last.request.content)
    assert body == {"confirm": True, "review_digest": _DIGEST}


@respx.mock
def test_get_plan_reads_review() -> None:
    respx.get(f"{BASE}/platform/config-plans/{_PLAN}").mock(
        return_value=httpx.Response(
            200, json=_envelope({"status": "accepted", "review_digest": _DIGEST})
        )
    )
    with Client(api_key="k", api_secret="s") as c:
        out = c.config.get_plan(_PLAN)
    assert out["status"] == "accepted"


@respx.mock
def test_apply_digest_mismatch_raises_api_error() -> None:
    respx.post(f"{BASE}/platform/config-plans/{_PLAN}/apply").mock(
        return_value=httpx.Response(
            409,
            json={
                "success": False,
                "error": {"code": "CONFLICT", "message": "Config review digest does not match."},
            },
        )
    )
    with Client(api_key="k", api_secret="s") as c, pytest.raises(ApiError) as excinfo:
        c.config.apply(_PLAN, "0" * 64)
    assert excinfo.value.code == "CONFLICT"


@pytest.mark.asyncio
@respx.mock
async def test_async_surface_matches_sync() -> None:
    respx.get(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/config").mock(
        return_value=httpx.Response(200, json=_envelope({"document_text": _DOC_TEXT}))
    )
    async with AsyncClient(api_key="k", api_secret="s") as c:
        out = await c.config.export(_DEPLOYMENT)
    assert out["document_text"] == _DOC_TEXT


@pytest.mark.parametrize(
    "bad",
    ["../orders", "dpl_" + "a" * 16 + "?x=1", "cplan_" + "a" * 24 + "/apply", "", "x\n", None],
)
def test_config_rejects_identifiers_before_transport(bad) -> None:
    with respx.mock(assert_all_called=False) as mock, Client(api_key="k", api_secret="s") as c:
        for call in [
            lambda: c.config.export(bad),
            lambda: c.config.prepare(bad, _DOC_TEXT),
            lambda: c.config.get_plan(bad),
            lambda: c.config.apply(bad, _DIGEST),
        ]:
            with pytest.raises(ValueError):
                call()
        assert len(mock.calls) == 0


@pytest.mark.asyncio
@pytest.mark.parametrize("bad", ["../orders", "dpl_" + "a" * 16 + "#fragment", "", None])
async def test_async_config_rejects_identifiers_before_transport(bad) -> None:
    with respx.mock(assert_all_called=False) as mock:
        async with AsyncClient(api_key="k", api_secret="s") as c:
            for call in [
                lambda: c.config.export(bad),
                lambda: c.config.prepare(bad, _DOC_TEXT),
                lambda: c.config.get_plan(bad),
                lambda: c.config.apply(bad, _DIGEST),
            ]:
                with pytest.raises(ValueError):
                    await call()
        assert len(mock.calls) == 0


@respx.mock
def test_config_rejects_digest_and_oversize_document_before_transport() -> None:
    with Client(api_key="k", api_secret="s") as c:
        with pytest.raises(ValueError):
            c.config.apply(_PLAN, "x\n" + _DIGEST)
        with pytest.raises(ValueError):
            c.config.prepare(_DEPLOYMENT, "é" * 32769)
    assert len(respx.calls) == 0


@respx.mock
def test_config_rejects_malformed_success_envelope() -> None:
    respx.get(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/config").respond(
        200, json={"success": True, "data": []}
    )
    with Client(api_key="k", api_secret="s") as c, pytest.raises(ApiError, match="data object"):
        c.config.export(_DEPLOYMENT)


@respx.mock
def test_config_accepts_extended_deployment_identifier() -> None:
    deployment = "dpl_" + "b" * 24
    respx.get(f"{BASE}/platform/deployments/custom/{deployment}/config").respond(
        200, json=_envelope({"document_text": _DOC_TEXT})
    )
    with Client(api_key="k", api_secret="s") as c:
        assert c.config.export(deployment)["document_text"] == _DOC_TEXT
