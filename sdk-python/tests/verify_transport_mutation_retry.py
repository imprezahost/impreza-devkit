"""Run SDK transport decisions against deterministic in-memory responses."""

from __future__ import annotations

import asyncio
import importlib
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import AsyncMock, Mock

# Load only the real SDK transports, with a socket-free HTTP boundary.
sdk = Path(__file__).resolve().parents[1] / "impreza"
package = types.ModuleType("impreza")
package.__path__ = [str(sdk)]
sys.modules["impreza"] = package
httpx = types.ModuleType("httpx")


class RequestError(Exception):
    """Synthetic connection failure."""


httpx.RequestError = RequestError
sys.modules["httpx"] = httpx
sync_transport = importlib.import_module("impreza._http")
async_transport = importlib.import_module("impreza._http_async")
errors = importlib.import_module("impreza.exceptions")


class Response:
    def __init__(self, status: int, payload: dict | None = None):
        self.status_code = status
        self.headers = {"Retry-After": "0"}
        self.payload = payload if payload is not None else {"success": True, "data": {"id": 13}}

    def json(self) -> dict:
        return self.payload


def make_transport(side_effect, *, asynchronous: bool):
    cls = async_transport.AsyncHttpClient if asynchronous else sync_transport.HttpClient
    sdk_transport = object.__new__(cls)
    sdk_transport._max_retries = 1
    sdk_transport._client = types.SimpleNamespace(
        request=(AsyncMock if asynchronous else Mock)(side_effect=side_effect)
    )
    sdk_transport._backoff = lambda _: 0
    return sdk_transport


class TestMutationRetry(unittest.TestCase):
    def test_mutations_do_not_replay_ambiguous_outcomes(self):
        for asynchronous in (False, True):
            for method in ("POST", "PUT", "DELETE"):
                for kind in ("timeout", "gateway_error"):
                    with self.subTest(asynchronous=asynchronous, method=method, kind=kind):
                        first = (
                            RequestError("synthetic read timeout")
                            if kind == "timeout"
                            else Response(502)
                        )
                        transport = make_transport(
                            [first, Response(200)], asynchronous=asynchronous
                        )
                        with self.assertRaises((errors.NetworkError, errors.UpstreamError)):
                            result = transport._request(method, "/fixture/mutation")
                            if asynchronous:
                                asyncio.run(result)
                        self.assertEqual(transport._client.request.call_count, 1)

    def test_reads_retry_and_confirmed_mutations_keep_one_call(self):
        for asynchronous in (False, True):
            for method in ("GET", "POST"):
                with self.subTest(asynchronous=asynchronous, method=method):
                    outcomes = (
                        [Response(502), Response(200)] if method == "GET" else [Response(200)]
                    )
                    transport = make_transport(outcomes, asynchronous=asynchronous)
                    result = transport._request(method, "/fixture/receipt")
                    if asynchronous:
                        result = asyncio.run(result)
                    self.assertEqual(result["data"], {"id": 13})
                    self.assertEqual(transport._client.request.call_count, len(outcomes))


class TestMutationReply(unittest.TestCase):
    def test_mutation_receipt_requires_explicit_protocol_success(self):
        cases = {
            "missing": Response(200, {"data": {"id": 13}}),
            "nonboolean": Response(200, {"success": "true", "data": {"id": 13}}),
            "contradiction": Response(200, {"success": True, "error": {"code": "NOT_CONFIRMED"}}),
            "redirect": Response(302),
            "confirmed": Response(200),
        }
        for asynchronous in (False, True):
            for name, response in cases.items():
                with self.subTest(asynchronous=asynchronous, name=name):

                    def request(transport, asynchronous=asynchronous):
                        result = transport._request("POST", "/fixture/mutation")
                        return asyncio.run(result) if asynchronous else result

                    transport = make_transport([response], asynchronous=asynchronous)
                    if name == "confirmed":
                        self.assertEqual(request(transport)["data"], {"id": 13})
                    else:
                        with self.assertRaises(errors.ApiError):
                            request(transport)
                    self.assertEqual(transport._client.request.call_count, 1)


if __name__ == "__main__":
    unittest.main()
