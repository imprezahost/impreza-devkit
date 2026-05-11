"""Live integration smoke tests for Phase 1.4b-i (vps common).

Only operations with **no side effects** run by default:

* ``c.vps.list()`` — a read across both backends.
* ``vps.status()`` for each listed VPS — read-only metric fetch.

Mutating operations (start/stop/reboot/shutdown, set_hostname,
set_password, reinstall) are covered by the mocked unit suite. Driving
them against a real VPS would require an opt-in flag and a designated
throwaway service id; that pattern is reserved for a future destructive
smoke suite.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_1_4b_smoke.py -v -s
"""

from __future__ import annotations

from impreza import Client, Vps, VpsStatus


def test_smoke_vps_list_works_against_live_api(live_client: Client) -> None:
    """``c.vps.list()`` should return :class:`Vps` instances with valid backends.

    Allowed to return an empty list (the test account may not have any VPS
    services). The contract being verified here is just that the call
    succeeds and the response is shaped correctly.
    """
    vpss = live_client.vps.list()

    assert isinstance(vpss, list)
    for vps in vpss:
        assert isinstance(vps, Vps)
        assert vps.backend in ("proxmox", "cloud")
        assert vps.id > 0

    backends = {v.backend for v in vpss}
    print(f"\n  found {len(vpss)} VPS service(s); backends present: {sorted(backends)}")


def test_smoke_status_fetch_for_each_listed_vps(live_client: Client) -> None:
    """Fetching ``status()`` for every listed VPS should succeed.

    Skips silently when the account has no VPS — the assertion that
    matters here is that the dispatch picks the right URL per backend
    and the response decodes into :class:`VpsStatus`.
    """
    vpss = live_client.vps.list()
    if not vpss:
        print("\n  no VPS services on this account — nothing to probe")
        return

    for vps in vpss:
        status = vps.status()
        assert isinstance(status, VpsStatus)
        assert isinstance(status.power_state, str) and status.power_state != ""
        print(
            f"\n  vps {vps.id} ({vps.backend}): power_state={status.power_state}"
            + (f", uptime={status.uptime}s" if status.uptime is not None else "")
        )
