"""Regenerate every QuantConnect script behind
docs/research/boost-followup-2026-09.md from the templates in boost/ and
shorting/. The repository never stores market or economic data, so the
templates carry a RATES placeholder, filled here from a local copy of FRED's
TB3MS monthly CSV (https://fred.stlouisfed.org/series/TB3MS).

RATES maps YYYYMM -> the PREVIOUS month's TB3MS average / 100 (no
look-ahead), from FIRST_MONTH on. Each variant is its template with the
class constants listed in VARIANTS replaced.

Usage: python3 build_variants.py TB3MS.csv OUT_DIR
       writes OUT_DIR/<variant>/main.py for every variant.
"""
import csv
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
FIRST_MONTH = 199201
PLACEHOLDER = "RATES = __RATES__"

IS_END = (2015, 12, 31)
_MIX = {"MODE": "mix"}
_OOS = {"START_DATE": (2015, 1, 1), "END_DATE": (2026, 6, 30), "MEASURE_FROM": (2016, 1, 1), "TOP_N": 25}
_HALF = {"MODE": "mom_trend", "TOP_N": 25, "RISK_OFF_FRACTION": 0.5}
_COMBO = dict(_HALF, SECTOR_CAP=3, INV_VOL=True)
_M1 = {"SPREAD": 0.022, "FEES": "none"}
# Glide path (docs/live-trading/plan.md, section 2): the combination plus a
# half-IEF/SHY sleeve in one account. Tuning window 2002-08 to 2015-12 (the
# bond funds start in July 2002), held-out window 2016-01 to 2026-09.
_GLIDE_IS = dict(_COMBO, START_DATE=(2001, 8, 1), MEASURE_FROM=(2002, 8, 1), END_DATE=(2015, 12, 31),
                 BOND_FUNDS={"IEF": 0.5, "SHY": 0.5})
_GLIDE_OOS = dict(_COMBO, START_DATE=(2015, 1, 1), MEASURE_FROM=(2016, 1, 1), END_DATE=(2026, 9, 30),
                  BOND_FUNDS={"IEF": 0.5, "SHY": 0.5})
# Universe parity (docs/live-trading/plan.md, section 4): the combination's
# held-out settings, run to September 2026.
_PARITY = dict(_OOS, **_COMBO, END_DATE=(2026, 9, 30), ETF_TICKER="IWB")
#: Sector check (docs/research/sector-labels-2026-10.md): today's universe
#: stocks whose GICS sector, mapped onto Morningstar's codes, differs.
_GICS_RELABEL = {"ADP": 310, "AGNC": 103, "AMCR": 101, "ASTS": 308, "CASY": 205, "CLSK": 311, "CRCL": 311,
                 "FISV": 103, "HUT": 311, "IREN": 311, "MARA": 311, "MOD": 310, "NXT": 310, "PAYX": 310,
                 "RIOT": 311, "ROL": 310, "SPCX": 308, "TOST": 103, "UBER": 310, "XYZ": 103}
_CTRY_OOS = {"MODE": "country_rot", "START": (2015, 1, 1), "END": (2026, 6, 30), "MEASURE_FROM": (2016, 1, 1)}
_US3X = {"SPY": 1, "MDY": 1, "IWM": 1, "EFA": 1}
_D11, _D19, _D21, _D22, _D23 =(2011, 3, 1), (2019, 7, 1), (2021, 2, 1), (2022, 5, 1), (2023, 11, 1)

#: variant -> (template, {class constant: value}), exactly as run.
VARIANTS = {
    # Boost deep-dive
    "BX_SPY": ("boost/template.py", {}),
    "BX_SPY_BH": ("boost/template.py", {"MODE": "bh"}),
    # Golden cases for the Go module (ADR 0026): BX_SPY unchanged, with each
    # day of three pullback months in uptrends, and the first decision, logged.
    "BX_GOLD": ("boost/golden_log.py", {"GOLDEN_MONTHS": ((2017, 8), (2019, 5), (2024, 4))}),
    "BX_SPY_C105": ("boost/template.py", {"MODE": "const", "CONST_LEV": 1.055}),
    "BX_SPY_C110": ("boost/template.py", {"MODE": "const", "CONST_LEV": 1.10}),
    "BX_QQQ": ("boost/template.py", {"TICKER": "QQQ"}),
    "BX_IWM": ("boost/template.py", {"TICKER": "IWM"}),
    "BX_DIA": ("boost/template.py", {"TICKER": "DIA"}),
    "BX_EFA": ("boost/template.py", {"TICKER": "EFA"}),
    "BX_MDY": ("boost/template.py", {"TICKER": "MDY"}),
    # Boost across several markets: the rule unchanged, no tuning
    "MB_US3": ("boost/multi.py", {}),
    "MB_US3_BH": ("boost/multi.py", {"MODE": "bh"}),
    "MB_US3X": ("boost/multi.py", {"SLEEVES": _US3X}),
    "MB_US3X_BH": ("boost/multi.py", {"SLEEVES": _US3X, "MODE": "bh"}),
    "MB_SPY": ("boost/multi.py", {"SLEEVES": {"SPY": 1}}),
    "MB_SPY_BH": ("boost/multi.py", {"SLEEVES": {"SPY": 1}, "MODE": "bh"}),
    # Bond funds held, for momentum-plus-bonds retirement mixes (portfolio/bonds.json):
    # the multi-fund holder with one sleeve, no rule to tune
    "MB_IEF_BH": ("boost/multi.py", {"SLEEVES": {"IEF": 1}, "MODE": "bh"}),
    "MB_TLT_BH": ("boost/multi.py", {"SLEEVES": {"TLT": 1}, "MODE": "bh"}),
    "MB_SHY_BH": ("boost/multi.py", {"SLEEVES": {"SHY": 1}, "MODE": "bh"}),
    "BX_SLIP5": ("boost/template.py", {"SLIPPAGE": 0.0005}),
    "BX_SLIP10": ("boost/template.py", {"SLIPPAGE": 0.001}),
    "BX_SPR3": ("boost/template.py", {"SPREAD": 0.03}),
    "BX_SPR6": ("boost/template.py", {"SPREAD": 0.06}),
    "BX_SPR9": ("boost/template.py", {"SPREAD": 0.09}),
    "BX_FUT": ("boost/template.py", {"SPREAD": 0.004}),
    "BR_S125": ("boost/template.py", {"END": IS_END, "BOOST_SIZE": 1.25}),
    "BR_S200": ("boost/template.py", {"END": IS_END, "BOOST_SIZE": 2.0}),
    "BR_E5": ("boost/template.py", {"END": IS_END, "RSI_ENTRY": 5}),
    "BR_E15": ("boost/template.py", {"END": IS_END, "RSI_ENTRY": 15}),
    "BR_E20": ("boost/template.py", {"END": IS_END, "RSI_ENTRY": 20}),
    "BR_NOTREND": ("boost/template.py", {"END": IS_END, "TREND_DAYS": 0}),
    "BR_X3": ("boost/template.py", {"END": IS_END, "EXIT_SMA": 3}),
    "BR_X10": ("boost/template.py", {"END": IS_END, "EXIT_SMA": 10}),
    # Shorting
    "SH_TR_CASH": ("shorting/trend_template.py", {"MODE": "trend_cash", "END": IS_END}),
    "SH_TR_SHORT": ("shorting/trend_template.py", {"MODE": "trend_short", "END": IS_END}),
    "SH_TR_CASH_QQQ": ("shorting/trend_template.py", {"MODE": "trend_cash", "END": IS_END, "TICKER": "QQQ"}),
    "SH_TR_SHORT_QQQ": ("shorting/trend_template.py", {"MODE": "trend_short", "END": IS_END, "TICKER": "QQQ"}),
    "SH_MOM_LONG25": ("shorting/factors_ls.py", {"MODE": "mom", "TOP_N": 25}),
    "SH_MOM_LS": ("shorting/factors_ls.py", {"MODE": "mom_ls", "TOP_N": 25}),
    "SH_MOM_130": ("shorting/factors_ls.py", {"MODE": "mom_130", "TOP_N": 25}),
    # Momentum held-out runs (2016 on, run once with the 1999-2015 settings), and cost stress
    "OOS_MOM_130": ("shorting/factors_ls.py", dict(_OOS, MODE="mom_130")),
    "OOS_MOM_LONG25": ("shorting/factors_ls.py", dict(_OOS, MODE="mom")),
    "OOS_130_S10_B5": ("shorting/factors_ls.py", dict(_OOS, MODE="mom_130", SLIPPAGE=0.001, BORROW_FEE=0.05)),
    "OOS_130_S20_B5": ("shorting/factors_ls.py", dict(_OOS, MODE="mom_130", SLIPPAGE=0.002, BORROW_FEE=0.05)),
    "OOS_L25_S10": ("shorting/factors_ls.py", dict(_OOS, MODE="mom", SLIPPAGE=0.001)),
    "OOS_L25_S20": ("shorting/factors_ls.py", dict(_OOS, MODE="mom", SLIPPAGE=0.002)),
    # Boost without borrowing (a leveraged S&P 500 fund in place of margin), and
    # margin Boost over the same spans for comparison
    "BE_SSO": ("boost/lev_etf_template.py", {"MODE": "boost_etf", "START": (2006, 7, 1)}),
    "BE_SPY_06": ("boost/lev_etf_template.py", {"START": (2006, 7, 1)}),
    "BE_UPRO": ("boost/lev_etf_template.py", {"MODE": "boost_etf", "START": (2009, 7, 1),
                                             "LEV_ETF": "UPRO", "LEV_FACTOR": 3.0}),
    "BE_SPY_09": ("boost/lev_etf_template.py", {"START": (2009, 7, 1)}),
    # Momentum robustness, tuning years only: names x band (B) or entries/exits
    # only (N) x trend filter (T), long-only; MG_25_B reproduces SH_MOM_LONG25
    "MG_15_B": ("momentum/momentum.py", {"MODE": "mom", "TOP_N": 15}),
    "MG_15_BT": ("momentum/momentum.py", {"MODE": "mom_trend", "TOP_N": 15}),
    "MG_15_N": ("momentum/momentum.py", {"MODE": "mom", "TOP_N": 15, "REBALANCE_BAND": None}),
    "MG_15_NT": ("momentum/momentum.py", {"MODE": "mom_trend", "TOP_N": 15, "REBALANCE_BAND": None}),
    "MG_25_B": ("momentum/momentum.py", {"MODE": "mom", "TOP_N": 25}),
    "MG_25_BT": ("momentum/momentum.py", {"MODE": "mom_trend", "TOP_N": 25}),
    "MG_25_N": ("momentum/momentum.py", {"MODE": "mom", "TOP_N": 25, "REBALANCE_BAND": None}),
    "MG_25_NT": ("momentum/momentum.py", {"MODE": "mom_trend", "TOP_N": 25, "REBALANCE_BAND": None}),
    "MG_50_B": ("momentum/momentum.py", {"MODE": "mom", "TOP_N": 50}),
    "MG_50_BT": ("momentum/momentum.py", {"MODE": "mom_trend", "TOP_N": 50}),
    "MG_50_N": ("momentum/momentum.py", {"MODE": "mom", "TOP_N": 50, "REBALANCE_BAND": None}),
    "MG_50_NT": ("momentum/momentum.py", {"MODE": "mom_trend", "TOP_N": 50, "REBALANCE_BAND": None}),
    # The tuning-year winner of the grid above, run once on the held-out years
    "OOS_MG_25_BT": ("momentum/momentum.py", dict(_OOS, MODE="mom_trend")),
    # Momentum with drawdown controls, tuning years only, 25 names, banded;
    # D_BASE has every control off and must reproduce MG_25_B
    "D_BASE": ("momentum/defensive.py", {"MODE": "mom", "TOP_N": 25}),
    "D_SEC3": ("momentum/defensive.py", {"MODE": "mom", "TOP_N": 25, "SECTOR_CAP": 3}),
    "D_SEC2": ("momentum/defensive.py", {"MODE": "mom", "TOP_N": 25, "SECTOR_CAP": 2}),
    "D_RISKADJ": ("momentum/defensive.py", {"MODE": "mom", "TOP_N": 25, "SCORE": "ret_vol"}),
    "D_INVVOL": ("momentum/defensive.py", {"MODE": "mom", "TOP_N": 25, "INV_VOL": True}),
    "D_STOCK200": ("momentum/defensive.py", {"MODE": "mom", "TOP_N": 25, "STOCK_TREND_DAYS": 200}),
    "D_HALF": ("momentum/defensive.py", {"MODE": "mom_trend", "TOP_N": 25, "RISK_OFF_FRACTION": 0.5}),
    "D_SPY50": ("momentum/defensive.py", {"MODE": "mom", "TOP_N": 25, "SPY_BLEND": 0.5}),
    "D_COMBO": ("momentum/defensive.py", {"MODE": "mom_trend", "TOP_N": 25, "SECTOR_CAP": 3,
                                         "INV_VOL": True, "RISK_OFF_FRACTION": 0.5}),
    # The two drawdown-control leaders, run once on the held-out years
    "OOS_D_COMBO": ("momentum/defensive.py", dict(_OOS, MODE="mom_trend", SECTOR_CAP=3, INV_VOL=True,
                                                  RISK_OFF_FRACTION=0.5)),
    "OOS_D_HALF": ("momentum/defensive.py", dict(_OOS, MODE="mom_trend", RISK_OFF_FRACTION=0.5)),
    # The unpriced-buy fix (momentum/defensive_v2.py): the headline momentum
    # variants re-run, tuning years and held-out years
    "F_NOFILTER": ("momentum/defensive_v2.py", {"MODE": "mom", "TOP_N": 25}),
    "F_FULL": ("momentum/defensive_v2.py", {"MODE": "mom_trend", "TOP_N": 25}),
    "F_HALF": ("momentum/defensive_v2.py", {"MODE": "mom_trend", "TOP_N": 25, "RISK_OFF_FRACTION": 0.5}),
    "F_COMBO": ("momentum/defensive_v2.py", {"MODE": "mom_trend", "TOP_N": 25, "SECTOR_CAP": 3,
                                            "INV_VOL": True, "RISK_OFF_FRACTION": 0.5}),
    "OOS_F_NOFILTER": ("momentum/defensive_v2.py", dict(_OOS, MODE="mom")),
    "OOS_F_FULL": ("momentum/defensive_v2.py", dict(_OOS, MODE="mom_trend")),
    "OOS_F_HALF": ("momentum/defensive_v2.py", dict(_OOS, MODE="mom_trend", RISK_OFF_FRACTION=0.5)),
    "OOS_F_COMBO": ("momentum/defensive_v2.py", dict(_OOS, MODE="mom_trend", SECTOR_CAP=3, INV_VOL=True,
                                                    RISK_OFF_FRACTION=0.5)),
    # 130/30 re-run with the fix: the same settings as SH_MOM_130, OOS_MOM_130
    # and its two cost-stress runs, on the fixed template
    "F_130": ("momentum/defensive_v2.py", {"MODE": "mom_130", "TOP_N": 25}),
    "OOS_F_130": ("momentum/defensive_v2.py", dict(_OOS, MODE="mom_130")),
    "OOS_F_130_S10_B5": ("momentum/defensive_v2.py", dict(_OOS, MODE="mom_130", SLIPPAGE=0.001, BORROW_FEE=0.05)),
    "OOS_F_130_S20_B5": ("momentum/defensive_v2.py", dict(_OOS, MODE="mom_130", SLIPPAGE=0.002, BORROW_FEE=0.05)),
    # Execution test (M1 against Interactive Brokers): the half-filter and
    # combination books of F_HALF / F_COMBO and their held-out runs, traded
    # 1 or 3 sessions after the month's book is computed.
    "XF_HALF_D3": ("momentum/execution.py", dict(_HALF, EXEC_DELAY=3)),
    "XF_COMBO_D3": ("momentum/execution.py", dict(_COMBO, EXEC_DELAY=3)),
    "OOS_XF_HALF_D1": ("momentum/execution.py", dict(_OOS, **_HALF, EXEC_DELAY=1)),
    "OOS_XF_HALF_D3": ("momentum/execution.py", dict(_OOS, **_HALF, EXEC_DELAY=3)),
    "OOS_XF_COMBO_D1": ("momentum/execution.py", dict(_OOS, **_COMBO, EXEC_DELAY=1)),
    "OOS_XF_COMBO_D3": ("momentum/execution.py", dict(_OOS, **_COMBO, EXEC_DELAY=3)),
    # Boost on SPY minute data: when each decision is made and filled, the
    # margin spread (IB about 1.5%, M1 5.90% less the T-bill, about 2.2%)
    # and commissions (IB's, or none at M1).
    # Universe parity: the backtest's own universe, then IWB's holdings at
    # least 7 days old and, for the sensitivity, 1 day old.
    "OOS_P_COARSE": ("momentum/universe_parity.py", dict(_PARITY, UNIVERSE_SOURCE="coarse", ETF_LAG_DAYS=7)),
    "OOS_P_IWB7": ("momentum/universe_parity.py", dict(_PARITY, UNIVERSE_SOURCE="etf", ETF_LAG_DAYS=7)),
    "OOS_P_IWB1": ("momentum/universe_parity.py", dict(_PARITY, UNIVERSE_SOURCE="etf", ETF_LAG_DAYS=1)),
    # Sector check: the backtest's own universe with the 20 stocks whose GICS
    # sector (iShares IWV, 2026-10-14, mapped onto Morningstar's) differs from
    # Morningstar's relabelled the GICS way.
    "OOS_P_GICS": ("momentum/universe_parity.py", dict(_PARITY, UNIVERSE_SOURCE="coarse", ETF_LAG_DAYS=7,
                                                       SECTOR_OVERRIDE=_GICS_RELABEL)),
    # Glide path: the schedule started at ages 38, 53 and 65 on each window's
    # first date, and the fixed mixes either side of each run (plan section 2).
    "G_IS_P38": ("momentum/glide_path.py", dict(_GLIDE_IS, GLIDE="path", BOND_SHARE=0.0, PATH_START_AGE=38.0)),
    "G_IS_P53": ("momentum/glide_path.py", dict(_GLIDE_IS, GLIDE="path", BOND_SHARE=0.0, PATH_START_AGE=53.0)),
    "G_IS_P65": ("momentum/glide_path.py", dict(_GLIDE_IS, GLIDE="path", BOND_SHARE=0.0, PATH_START_AGE=65.0)),
    "G_IS_F00": ("momentum/glide_path.py", dict(_GLIDE_IS, GLIDE="fixed", BOND_SHARE=0.0, PATH_START_AGE=38.0)),
    "G_IS_F30": ("momentum/glide_path.py", dict(_GLIDE_IS, GLIDE="fixed", BOND_SHARE=0.3, PATH_START_AGE=38.0)),
    "G_IS_F50": ("momentum/glide_path.py", dict(_GLIDE_IS, GLIDE="fixed", BOND_SHARE=0.5, PATH_START_AGE=38.0)),
    "G_IS_F60": ("momentum/glide_path.py", dict(_GLIDE_IS, GLIDE="fixed", BOND_SHARE=0.6, PATH_START_AGE=38.0)),
    "G_IS_F70": ("momentum/glide_path.py", dict(_GLIDE_IS, GLIDE="fixed", BOND_SHARE=0.7, PATH_START_AGE=38.0)),
    "G_OOS_P38": ("momentum/glide_path.py", dict(_GLIDE_OOS, GLIDE="path", BOND_SHARE=0.0, PATH_START_AGE=38.0)),
    "G_OOS_P53": ("momentum/glide_path.py", dict(_GLIDE_OOS, GLIDE="path", BOND_SHARE=0.0, PATH_START_AGE=53.0)),
    "G_OOS_P65": ("momentum/glide_path.py", dict(_GLIDE_OOS, GLIDE="path", BOND_SHARE=0.0, PATH_START_AGE=65.0)),
    "G_OOS_F00": ("momentum/glide_path.py", dict(_GLIDE_OOS, GLIDE="fixed", BOND_SHARE=0.0, PATH_START_AGE=38.0)),
    "G_OOS_F30": ("momentum/glide_path.py", dict(_GLIDE_OOS, GLIDE="fixed", BOND_SHARE=0.3, PATH_START_AGE=38.0)),
    "G_OOS_F50": ("momentum/glide_path.py", dict(_GLIDE_OOS, GLIDE="fixed", BOND_SHARE=0.5, PATH_START_AGE=38.0)),
    "G_OOS_F70": ("momentum/glide_path.py", dict(_GLIDE_OOS, GLIDE="fixed", BOND_SHARE=0.7, PATH_START_AGE=38.0)),
    "BE_IB_MOC": ("boost/execution.py", {"EXEC": "moc"}),
    "BE_IB_OPEN": ("boost/execution.py", {"EXEC": "next_open"}),
    "BE_M1_AM": ("boost/execution.py", dict(_M1, EXEC="next_open")),
    "BE_M1_PM": ("boost/execution.py", dict(_M1, EXEC="afternoon")),
    "BE_M1_AM_D1": ("boost/execution.py", dict(_M1, EXEC="next_open", EXEC_DELAY=1)),
    "BE_BH": ("boost/execution.py", {"MODE": "bh"}),
    # Other candidates, tuning years only. S_BASE must reproduce MG_25_B.
    "S_BASE": ("momentum/scores.py", {"MODE": "mom", "TOP_N": 25}),
    "S_HIGH52": ("momentum/scores.py", {"MODE": "mom", "TOP_N": 25, "SCORE": "high52"}),
    "S_INTER": ("momentum/scores.py", {"MODE": "mom", "TOP_N": 25, "SCORE": "inter"}),
    "S_FIP": ("momentum/scores.py", {"MODE": "mom", "TOP_N": 25, "SCORE": "fip"}),
    "S_RESID": ("momentum/scores.py", {"MODE": "mom", "TOP_N": 25, "SCORE": "resid"}),
    "E_SECROT": ("etf/rotation.py", {"MODE": "sector_rot", "MEASURE_FROM": (2000, 1, 1)}),
    "E_SECROT_T": ("etf/rotation.py", {"MODE": "sector_rot", "SMA_MONTHS": 10, "MEASURE_FROM": (2000, 1, 1)}),
    "E_LEV2": ("etf/rotation.py", {"MODE": "lev_trend", "LEVERAGE": 2.0}),
    "E_LEV1": ("etf/rotation.py", {"MODE": "lev_trend", "LEVERAGE": 1.0}),
    # Country momentum, tuning years (1999-2015)
    "E_CTRY_T3": ("etf/rotation.py", {"MODE": "country_rot", "TOP": 3}),
    "E_CTRY_T5": ("etf/rotation.py", {"MODE": "country_rot", "TOP": 5}),
    "E_CTRY_T3_S10": ("etf/rotation.py", {"MODE": "country_rot", "TOP": 3, "SMA_MONTHS": 10}),
    "E_CTRY_T5_S10": ("etf/rotation.py", {"MODE": "country_rot", "TOP": 5, "SMA_MONTHS": 10}),
    "E_CTRY_EW": ("etf/rotation.py", {"MODE": "country_rot", "TOP": 17}),
    # Country momentum, held-out years: the tuning years' best setting, run once
    "E_CTRY_OOS_T5_S10": ("etf/rotation.py", dict(_CTRY_OOS, TOP=5, SMA_MONTHS=10)),
    "E_CTRY_OOS_EW": ("etf/rotation.py", dict(_CTRY_OOS, TOP=17)),
    # Momentum crash guard (volatility scaling), tuning years only
    "MV_L25_20": ("momentum/momentum.py", {"MODE": "mom", "TOP_N": 25, "VOL_TARGET": 0.20}),
    "MV_L25_30": ("momentum/momentum.py", {"MODE": "mom", "TOP_N": 25, "VOL_TARGET": 0.30}),
    "MV_130_20": ("momentum/momentum.py", {"MODE": "mom_130", "TOP_N": 25, "VOL_TARGET": 0.20}),
    "MV_130_30": ("momentum/momentum.py", {"MODE": "mom_130", "TOP_N": 25, "VOL_TARGET": 0.30}),
    # Managed-futures ETFs
    "M_DBMF": ("boost/mix_template.py", dict(_MIX, START=_D19, WEIGHTS={"DBMF": 1.0}, TICKER="SPY")),
    "M_8020_DBMF": ("boost/mix_template.py", dict(_MIX, START=_D19, WEIGHTS={"SPY": 0.8, "DBMF": 0.2})),
    "M_150_DBMF": ("boost/mix_template.py", dict(_MIX, START=_D19, WEIGHTS={"SPY": 1.0, "DBMF": 0.5})),
    "M_BOOST_DBMF": ("boost/mix_template.py", dict(_MIX, START=_D19, WEIGHTS={"SPY": 1.0, "DBMF": 0.5}, BOOST=True)),
    "M_BOOST_19": ("boost/mix_template.py", dict(_MIX, START=_D19, WEIGHTS={"SPY": 1.0}, BOOST=True)),
    "M_150_KMLM": ("boost/mix_template.py", dict(_MIX, START=_D21, WEIGHTS={"SPY": 1.0, "KMLM": 0.5})),
    "M_150_CTA": ("boost/mix_template.py", dict(_MIX, START=_D22, WEIGHTS={"SPY": 1.0, "CTA": 0.5})),
    "M_WTMF": ("boost/mix_template.py", dict(_MIX, START=_D11, WEIGHTS={"WTMF": 1.0})),
    "M_8020_WTMF": ("boost/mix_template.py", dict(_MIX, START=_D11, WEIGHTS={"SPY": 0.8, "WTMF": 0.2})),
    "M_150_WTMF": ("boost/mix_template.py", dict(_MIX, START=_D11, WEIGHTS={"SPY": 1.0, "WTMF": 0.5})),
    "M_BOOST_WTMF": ("boost/mix_template.py", dict(_MIX, START=_D11, WEIGHTS={"SPY": 1.0, "WTMF": 0.5}, BOOST=True)),
    "M_BOOST_11": ("boost/mix_template.py", dict(_MIX, START=_D11, WEIGHTS={"SPY": 1.0}, BOOST=True)),
    "M_RSST": ("boost/mix_template.py", dict(_MIX, START=_D23, WEIGHTS={"RSST": 1.0})),
}


def prior_month_rates(tb3ms_path, first_month=FIRST_MONTH):
    """{YYYYMM: previous month's TB3MS / 100} from ``first_month`` on.
    Refuses a file without the rate ``first_month`` needs."""
    rates = {}
    with open(tb3ms_path) as f:
        for row in csv.reader(f):
            if not row or row[0].startswith("observation") or row[1] == ".":
                continue
            y, m, _ = (int(x) for x in row[0].split("-"))
            y, m = (y + 1, 1) if m == 12 else (y, m + 1)
            if y * 100 + m >= first_month:
                rates[y * 100 + m] = float(row[1]) / 100
    if first_month not in rates:
        raise SystemExit("TB3MS has no rate for the month before {}".format(first_month))
    return rates


def render(template_src, params, rates):
    src = template_src
    if src.count(PLACEHOLDER) > 1:
        raise SystemExit("the rate placeholder appears more than once; only the table line may carry it")
    if PLACEHOLDER in src:
        table = "{" + ", ".join("%d: %.4f" % (k, v) for k, v in sorted(rates.items())) + "}"
        src = src.replace(PLACEHOLDER, "RATES = " + table, 1)
    for name, value in params.items():
        src, n = re.subn(r"^(    %s = )[^\n#]*" % name, lambda m: m.group(1) + repr(value) + "  ",
                         src, count=1, flags=re.M)
        if n != 1:
            raise SystemExit("class constant {} not found".format(name))
    return src


#: Templates whose long-only modes ignore VOL_TARGET: their book comes from
#: defensive_book, which has no volatility scaling.
NO_VOL_TARGET = ("momentum/defensive.py", "momentum/defensive_v2.py", "momentum/scores.py")


def check_variant(template, params):
    if template in NO_VOL_TARGET and params.get("VOL_TARGET") and params.get("MODE", "mom") in ("mom", "mom_trend"):
        raise SystemExit("{} ignores VOL_TARGET in mode {}".format(template, params.get("MODE", "mom")))


def build_all(tb3ms_path, out_dir):
    rates = prior_month_rates(tb3ms_path)
    for name, (template, params) in VARIANTS.items():
        check_variant(template, params)
        with open(os.path.join(HERE, template)) as f:
            src = render(f.read(), params, rates)
        os.makedirs(os.path.join(out_dir, name), exist_ok=True)
        with open(os.path.join(out_dir, name, "main.py"), "w") as f:
            f.write(src)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    build_all(sys.argv[1], sys.argv[2])
