"""Capture the shapes ibapi hands the adapter for a contract's details and an account value, from ibapi's own decoder.

    PYTHONPATH=<ibapi's pythonclient> python3 capture_ibapi_shapes.py > testdata/ibapi_shapes.json

docs/live-trading/lessons-2026-10.md L5: the adapter's test doubles are
built from what ibapi delivers, never from what we expect it to. This
script feeds IBKR's wire messages, filled with synthetic values (an
invented symbol, contract ID and account), through the pinned ibapi's own
Decoder (deploy/versions.env, TWSAPI_VERSION; protobuf PROTOBUF_VERSION),
both the protobuf messages a Gateway that negotiates server version 205 or
later sends (the "protobuf" section) and the older text messages one that
negotiates a lower version sends (the "text" section, at version 200), and
records each callback's arguments as
ibapi passes them to EWrapper: every attribute the adapter reads, with its
Python type and value. No IBKR source is copied: only the field names and
types ibapi produces (ADR 0025, amendment of 2026-10-16). It needs ibapi
and protobuf, so it is run by hand (as on 2026-10-29, from the pinned
archive checked against TWSAPI_ARCHIVE_SHA256); the committed output is
what adapter/ibkr/tests/fake_tws.py builds its contract details and
account values from, and test_ira.py checks the two agree.
"""
import json
import sys

from ibapi import server_versions
from ibapi.decoder import Decoder
from ibapi.protobuf.AccountValue_pb2 import AccountValue
from ibapi.protobuf.ContractData_pb2 import ContractData
from ibapi.protobuf.ContractDataEnd_pb2 import ContractDataEnd
from ibapi.protobuf.ErrorMessage_pb2 import ErrorMessage
from ibapi.wrapper import EWrapper

# The attributes of a contract's details the adapter reads (snapshot.resolve_contracts).
CONTRACT_FIELDS = ("conId", "symbol", "secType", "exchange", "primaryExchange", "currency", "localSymbol",
                   "tradingClass")
DETAILS_FIELDS = ("marketName", "minTick", "longName", "stockType", "validExchanges")
# Synthetic values: an invented stock, contract ID and paper account.
SYMBOL, CON_ID, ACCOUNT = "AAA", 900001, "DU0000001"


class Recorder(EWrapper):
    def __init__(self):
        super().__init__()
        self.calls = []

    def contractDetails(self, reqId, contractDetails):
        self.calls.append(("contractDetails", reqId, contractDetails))

    def contractDetailsEnd(self, reqId):
        self.calls.append(("contractDetailsEnd", reqId))

    def updateAccountValue(self, key, val, currency, accountName):
        self.calls.append(("updateAccountValue", key, val, currency, accountName))

    def error(self, reqId, errorTime, errorCode, errorString, advancedOrderRejectJson=""):
        self.calls.append(("error", reqId, errorTime, errorCode, errorString))


def typed(value):
    return {"type": type(value).__name__, "value": value}


def details_shape(req_id, details):
    contract = details.contract
    return {"req_id": typed(req_id),
            "contract": {name: typed(getattr(contract, name)) for name in CONTRACT_FIELDS},
            "details": {name: typed(getattr(details, name)) for name in DETAILS_FIELDS}}


def protobuf_calls():
    wrapper = Recorder()
    decoder = Decoder(wrapper, server_versions.MIN_SERVER_VER_PROTOBUF_CONTRACT_DATA)
    data = ContractData()
    data.reqId = 7
    c = data.contract
    c.conId, c.symbol, c.secType, c.exchange, c.currency = CON_ID, SYMBOL, "STK", "SMART", "USD"
    c.localSymbol, c.tradingClass, c.primaryExch = SYMBOL, "NMS", "NASDAQ"
    d = data.contractDetails
    d.marketName, d.minTick, d.longName, d.stockType = "NMS", "0.01", "SYNTHETIC CO", "COMMON"
    d.validExchanges = "SMART,NASDAQ,ARCA"
    decoder.processContractDataMsgProtoBuf(data.SerializeToString())
    end = ContractDataEnd()
    end.reqId = 7
    decoder.processContractDataEndMsgProtoBuf(end.SerializeToString())
    value = AccountValue()
    value.key, value.value, value.currency, value.accountName = "SettledCash", "10000.00", "USD", ACCOUNT
    decoder.processAccountValueMsgProtoBuf(value.SerializeToString())
    refused = ErrorMessage()
    refused.id, refused.errorCode = 8, 200
    refused.errorMsg = "No security definition has been found for the request"
    refused.errorTime = 1800000000000
    decoder.processErrorMsgProtoBuf(refused.SerializeToString())
    return wrapper.calls


# A server version below protobuf (MIN_SERVER_VER_PROTOBUF, 201): a Gateway
# that negotiates one sends IBKR's older text messages, which ibapi decodes
# field by field (Decoder.interpret). At 200 every field the decoder reads
# for these messages is present (size rules, stock type, ineligibility
# reasons, error time).
TEXT_SERVER_VERSION = 200


def text_calls():
    """The same answers as text messages, each a list of fields as ibapi splits the socket's bytes."""
    from ibapi.message import IN
    wrapper = Recorder()
    decoder = Decoder(wrapper, TEXT_SERVER_VERSION)

    def b(*fields):
        return [str(f).encode() for f in fields]
    contract = b(
        7,                       # reqId
        SYMBOL, "STK", "",        # symbol, secType, lastTradeDateOrContractMonth
        "",                       # lastTradeDate
        0.0, "",                  # strike, right
        "SMART", "USD", SYMBOL,   # exchange, currency, localSymbol
        "NMS", "NMS", CON_ID,     # marketName, tradingClass, conId
        0.01, "", "LMT,MKT",      # minTick, multiplier, orderTypes
        "SMART,NASDAQ,ARCA", 1,   # validExchanges, priceMagnifier
        0, "SYNTHETIC CO", "NASDAQ",  # underConId, longName, primaryExchange
        "", "", "", "", "US/Eastern", "", "",  # contractMonth .. liquidHours
        "", 0, 0,                 # evRule, evMultiplier, secIdListCount
        1, "", "",                # aggGroup, underSymbol, underSecType
        "26", "", "COMMON",       # marketRuleIds, realExpirationDate, stockType
        "1", "1", "1",            # minSize, sizeIncrement, suggestedSizeIncrement
        0)                        # ineligibilityReasonListCount
    decoder.interpret(contract, IN.CONTRACT_DATA)
    decoder.interpret(b(1, 7), IN.CONTRACT_DATA_END)
    decoder.interpret(b(2, "SettledCash", "10000.00", "USD", ACCOUNT), IN.ACCT_VALUE)
    decoder.interpret(b(8, 200, "No security definition has been found for the request", "", 1800000000000),
                      IN.ERR_MSG)
    return wrapper.calls


def shapes(calls):
    out = {}
    for call in calls:
        if call[0] == "contractDetails":
            out["contract_details"] = details_shape(call[1], call[2])
        elif call[0] == "contractDetailsEnd":
            out["contract_details_end"] = {"req_id": typed(call[1])}
        elif call[0] == "updateAccountValue":
            out["account_value"] = {k: typed(v) for k, v in zip(("key", "val", "currency", "accountName"), call[1:])}
        elif call[0] == "error":
            out["error"] = {k: typed(v) for k, v in zip(("reqId", "errorTime", "errorCode", "errorString"), call[1:])}
    return out


def main():
    import ibapi
    report = {"ibapi": ".".join(str(p) for p in ibapi.VERSION.values()) if isinstance(ibapi.VERSION, dict)
              else str(ibapi.VERSION),
              "server_version": server_versions.MIN_SERVER_VER_PROTOBUF_CONTRACT_DATA,
              "protobuf": shapes(protobuf_calls()),
              "text_server_version": TEXT_SERVER_VERSION,
              "text": shapes(text_calls())}
    json.dump(report, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
