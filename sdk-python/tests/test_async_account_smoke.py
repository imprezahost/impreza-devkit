"""Live integration smoke test for ``AsyncClient`` (Phase 1.2).

Hits the real ``api.imprezahost.com`` over clearnet using credentials
from the environment. Skipped automatically when the credentials are
not set.

Run locally::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_async_account_smoke.py -v -s
"""

from __future__ import annotations

import os

import pytest

from impreza import AccountInfo, AsyncClient


@pytest.mark.asyncio
async def test_async_account_get_returns_real_profile() -> None:
    if not os.environ.get("IMPREZA_API_KEY") or not os.environ.get("IMPREZA_API_SECRET"):
        pytest.skip(
            "IMPREZA_API_KEY/IMPREZA_API_SECRET not set — skipping live integration test"
        )

    async with AsyncClient.from_env() as c:
        account = await c.account.get()

    assert isinstance(account, AccountInfo)
    assert account.id > 0
    assert account.email
    assert "@" in account.email
    assert account.currency
    assert isinstance(account.balance, float)

    print(
        f"\n  async account.id={account.id} email={account.email} "
        f"balance={account.balance} {account.currency}"
    )
