"""Live integration smoke test for Phase 1.1.

Hits the real ``api.imprezahost.com`` using credentials from the
environment. Skipped automatically when the credentials are not set, so
``pytest`` keeps passing on CI runners that have no secrets.

Run locally::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_account_smoke.py -v -s

The ``-s`` flag preserves prints, useful for eyeballing the returned
balance during the first manual run.
"""

from __future__ import annotations

from impreza import AccountInfo, Client


def test_account_get_returns_real_profile(live_client: Client) -> None:
    """``GET /account`` returns a populated profile and balance."""
    account = live_client.account.get()

    assert isinstance(account, AccountInfo)
    assert account.id > 0
    assert account.email
    assert "@" in account.email
    assert account.currency  # non-empty string

    # Balance can be 0.0 (string-empty accounts) but must be a real float.
    assert isinstance(account.balance, float)

    print(
        f"\n  account.id={account.id} email={account.email} "
        f"balance={account.balance} {account.currency}"
    )
