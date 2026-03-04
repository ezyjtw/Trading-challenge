#!/usr/bin/env python3
"""
Fetch current funding rates from Bybit V5 public API.
Displays top perpetual contracts by volume with annualized yield calculations.

Usage: python3 fetch_funding_rates.py
"""

import json
import urllib.request
import urllib.error
from datetime import datetime

URLS = {
    "all_linear": "https://api.bybit.com/v5/market/tickers?category=linear",
    "btcusdt": "https://api.bybit.com/v5/market/tickers?category=linear&symbol=BTCUSDT",
    "ethusdt": "https://api.bybit.com/v5/market/tickers?category=linear&symbol=ETHUSDT",
}


def fetch_json(url: str) -> dict:
    req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read().decode())


def fmt_number(n: float) -> str:
    if abs(n) >= 1e9:
        return f"{n / 1e9:.2f}B"
    if abs(n) >= 1e6:
        return f"{n / 1e6:.2f}M"
    if abs(n) >= 1e3:
        return f"{n / 1e3:.2f}K"
    return f"{n:.2f}"


def fmt_ts(ms_str: str) -> str:
    try:
        return datetime.utcfromtimestamp(int(ms_str) / 1000).strftime("%Y-%m-%d %H:%M UTC")
    except (ValueError, TypeError):
        return ms_str


def print_detailed(label: str, data: dict):
    """Print all fields for a single ticker."""
    print(f"\n{'=' * 80}")
    print(f"  {label} -- DETAILED RAW DATA")
    print(f"{'=' * 80}")
    ticker = data.get("result", {}).get("list", [{}])[0]
    for k, v in ticker.items():
        extra = ""
        if k == "fundingRate" and v:
            ann = float(v) * 3 * 365 * 100
            extra = f"  (annualized: {ann:+.2f}%)"
        if k == "nextFundingTime" and v:
            extra = f"  ({fmt_ts(v)})"
        print(f"  {k:<25} {v}{extra}")


def main():
    print("Fetching data from Bybit V5 API...")
    print(f"Timestamp: {datetime.utcnow().strftime('%Y-%m-%d %H:%M:%S UTC')}\n")

    # ---- Fetch all three endpoints ----
    try:
        all_data = fetch_json(URLS["all_linear"])
    except urllib.error.URLError as e:
        print(f"ERROR fetching all linear tickers: {e}")
        return

    try:
        btc_data = fetch_json(URLS["btcusdt"])
    except urllib.error.URLError as e:
        print(f"ERROR fetching BTCUSDT: {e}")
        btc_data = None

    try:
        eth_data = fetch_json(URLS["ethusdt"])
    except urllib.error.URLError as e:
        print(f"ERROR fetching ETHUSDT: {e}")
        eth_data = None

    # ---- Detailed BTC and ETH ----
    if btc_data:
        print_detailed("BTCUSDT Perpetual", btc_data)
    if eth_data:
        print_detailed("ETHUSDT Perpetual", eth_data)

    # ---- Top contracts by 24h turnover ----
    tickers = all_data.get("result", {}).get("list", [])
    perps = [
        t for t in tickers
        if t.get("symbol", "").endswith("USDT")
        and t.get("fundingRate")
    ]
    perps.sort(key=lambda x: float(x.get("turnover24h", 0)), reverse=True)

    top_n = 30
    print(f"\n{'=' * 130}")
    print(f"  TOP {top_n} USDT PERPETUAL CONTRACTS BY 24H TURNOVER  (total found: {len(perps)})")
    print(f"{'=' * 130}")

    header = (
        f"{'#':<4} {'Symbol':<16} {'Last Price':>14} {'24h Vol':>12} "
        f"{'24h Turnover':>14} {'Funding Rate':>14} {'Ann. Yield':>12} "
        f"{'Next Funding':>22} {'Open Interest':>16}"
    )
    print(header)
    print("-" * 130)

    for i, t in enumerate(perps[:top_n], 1):
        sym = t["symbol"]
        price = t.get("lastPrice", "N/A")
        vol = float(t.get("volume24h", 0))
        turnover = float(t.get("turnover24h", 0))
        fr = float(t.get("fundingRate", 0))
        ann = fr * 3 * 365 * 100
        nft = fmt_ts(t.get("nextFundingTime", ""))
        oi = float(t.get("openInterestValue", 0))

        sign = "+" if ann >= 0 else ""
        print(
            f"{i:<4} {sym:<16} {price:>14} {fmt_number(vol):>12} "
            f"{fmt_number(turnover):>14} {fr:>14.6f} {sign}{ann:>10.2f}% "
            f"{nft:>22} {fmt_number(oi):>16}"
        )

    # ---- Highlight key symbols ----
    key_symbols = ["BTCUSDT", "ETHUSDT", "SOLUSDT", "XRPUSDT", "DOGEUSDT",
                   "BNBUSDT", "ADAUSDT", "AVAXUSDT", "DOTUSDT", "LINKUSDT",
                   "MATICUSDT", "ARBUSDT", "OPUSDT", "SUIUSDT", "APTUSDT"]
    found = {t["symbol"]: t for t in perps if t["symbol"] in key_symbols}

    print(f"\n{'=' * 100}")
    print("  MAJOR CONTRACTS -- FUNDING RATE SUMMARY")
    print(f"{'=' * 100}")
    print(f"  {'Symbol':<16} {'Funding Rate':>14} {'Annualized':>14} {'Last Price':>14} {'24h Change':>12}")
    print(f"  {'-' * 70}")

    for sym in key_symbols:
        t = found.get(sym)
        if not t:
            continue
        fr = float(t.get("fundingRate", 0))
        ann = fr * 3 * 365 * 100
        price = t.get("lastPrice", "N/A")
        chg = float(t.get("price24hPcnt", 0)) * 100
        sign_ann = "+" if ann >= 0 else ""
        sign_chg = "+" if chg >= 0 else ""
        print(f"  {sym:<16} {fr:>14.6f} {sign_ann}{ann:>12.2f}% {price:>14} {sign_chg}{chg:>10.2f}%")

    # ---- Extremes ----
    print(f"\n{'=' * 100}")
    print("  EXTREME FUNDING RATES (highest & lowest)")
    print(f"{'=' * 100}")

    by_fr = sorted(perps, key=lambda x: float(x.get("fundingRate", 0)))

    print("\n  MOST NEGATIVE (shorts paying longs):")
    for t in by_fr[:10]:
        fr = float(t["fundingRate"])
        ann = fr * 3 * 365 * 100
        print(f"    {t['symbol']:<20} {fr:>14.6f}  ({ann:+.2f}% annualized)")

    print("\n  MOST POSITIVE (longs paying shorts):")
    for t in by_fr[-10:][::-1]:
        fr = float(t["fundingRate"])
        ann = fr * 3 * 365 * 100
        print(f"    {t['symbol']:<20} {fr:>14.6f}  ({ann:+.2f}% annualized)")

    print(f"\n{'=' * 100}")
    print("  Notes:")
    print("  - Funding rate is per 8-hour interval (3x daily)")
    print("  - Annualized yield = fundingRate * 3 * 365 * 100")
    print("  - Positive rate: longs pay shorts (bullish bias)")
    print("  - Negative rate: shorts pay longs (bearish bias)")
    print(f"{'=' * 100}")


if __name__ == "__main__":
    main()
