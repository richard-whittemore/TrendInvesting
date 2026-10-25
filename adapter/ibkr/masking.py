"""Account numbers masked out of text the adapter prints or logs.

A stop message or a log line is pasted into tickets and chats, and the
repository is public: no account number leaves the machine that way.
"""
import re

# An IBKR account number: one or two capital letters and digits (U1234567, DU1234567), or DU with letters and digits
# after it (DUX1234567); the letters-then-digits form alone would not match the second.
ACCOUNT_NUMBER = re.compile(r"\b(?:[A-Z]{1,2}[0-9]{5,9}|DU(?=[A-Z0-9]*[0-9])[A-Z0-9]{5,9})\b")


def mask_accounts(text, account_id):
    """text with the configured account and anything shaped like an IBKR account number replaced by <account>."""
    if account_id:
        text = text.replace(account_id, "<account>")
    return ACCOUNT_NUMBER.sub("<account>", text)
