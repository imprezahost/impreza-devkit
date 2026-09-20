"""Compatibility with the restricted Cloud status wire contract."""
from impreza.resources.vps import _cloud_status_from_payload


def test_basic_profile_status_keeps_allocated_memory_separate_from_usage():
    status = _cloud_status_from_payload({
        "success": True,
        "data": {
            "service_id": 101,
            "power_state": "online",
            "memory_total": 2147483648,
            "cpu_count": 2,
            "disk_total_gb": 40,
            "management_profile": "basic",
            "capabilities": ["status", "resources", "boot", "shutdown", "reboot"],
        },
    })
    assert status.power_state == "online"
    assert status.memory_total == 2147483648
    assert status.memory_used is None
    assert status.cpu_usage is None
