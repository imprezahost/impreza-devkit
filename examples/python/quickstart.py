"""Impreza SDK — quickstart (Phase 1+).

Currently a placeholder: the SDK is implemented in Phase 1 of the DevKit plan.
Once that lands, this script will run end-to-end.

Until then, see examples/curl/quickstart.sh for working examples.
"""

# from impreza import Client
#
# c = Client.from_env()
#
# me = c.account.get()
# print(f"Balance: {me.balance} {me.currency}")
#
# invoice = c.account.topup(amount=50, method="xmr")
# print("Pay at:", invoice.payment_url)
# invoice.wait_until_paid(timeout=7200)
#
# c.webhooks.create(
#     url="https://example.com/hooks",
#     events=["topup.paid", "vps.*"],
# )
