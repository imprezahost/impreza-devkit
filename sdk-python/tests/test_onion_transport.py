"""Constructor entry paths must refuse onion clearnet fallback before networking."""

from unittest.mock import patch

import pytest

from impreza import AsyncClient, Client

ONION = "http://" + "a" * 56 + ".onion/v1"


@pytest.mark.parametrize("client_class", [Client, AsyncClient])
@pytest.mark.parametrize(
    "url", [ONION, ONION.replace("http:", "https:"), ONION.replace(".onion/", ".onion./")]
)
def test_onion_without_explicit_proxy_is_rejected(client_class, url):
    with (
        patch.dict("os.environ", {"IMPREZA_USE_TOR": "0", "ALL_PROXY": "socks5://127.0.0.1:9050"}),
        pytest.raises(ValueError, match="requires an explicit SOCKS5"),
    ):
        client_class(api_key="fixture", api_secret="fixture", base_url=url)


@pytest.mark.parametrize("client_class", [Client, AsyncClient])
def test_auto_tor_cannot_fall_back_for_onion(client_class):
    with (
        patch.dict("os.environ", {"IMPREZA_USE_TOR": "0"}),
        patch("impreza._tor.is_tor_available", return_value=False),
        pytest.raises(ValueError, match="requires an explicit SOCKS5"),
    ):
        client_class(api_key="fixture", api_secret="fixture", base_url=ONION, auto_tor=True)


@pytest.mark.parametrize(
    "client_class,http_class", [(Client, "httpx.Client"), (AsyncClient, "httpx.AsyncClient")]
)
def test_explicit_onion_proxy_disables_environment_routes(client_class, http_class):
    with patch(http_class) as transport:
        client_class(
            api_key="fixture",
            api_secret="fixture",
            base_url=ONION,
            proxy="socks5h://127.0.0.1:9050",
        )
        assert transport.call_args.kwargs["proxy"] == "socks5://127.0.0.1:9050"
        assert transport.call_args.kwargs["trust_env"] is False


@pytest.mark.parametrize(
    "proxy",
    [
        "http://127.0.0.1:9050",
        "socks5://user:private@127.0.0.1:9050",
        "socks5://127.0.0.1:9050/path",
    ],
)
def test_onion_rejects_ambiguous_proxy_without_echoing_credentials(proxy):
    with pytest.raises(ValueError) as error:
        Client(api_key="fixture", api_secret="fixture", base_url=ONION, proxy=proxy)
    assert "private" not in str(error.value)
