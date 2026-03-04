# Trading SOP — HyroTrader Challenge

**One page. Follow it like a pilot's checklist. No exceptions.**

---

## Parameters (Hardcoded — No Changes Mid-Week)

| Parameter          | Value           | Rationale                                    |
|--------------------|-----------------|----------------------------------------------|
| Timeframe          | 15m             | Signal quality vs opportunity balance         |
| Markets            | BTCUSDT, ETHUSDT, SOLUSDT | 3 liquid, uncorrelated enough pairs  |
| Risk per trade     | 0.5% of account | 6 consecutive losses = 3% daily (safe)       |
| Max trades/day     | 3               | Prevents overtrading after wins or losses     |
| Daily stop-loss    | -1.5R ($750)    | Walk away. No coming back today.             |
| Consecutive losses | 2 → done for day| After 2 losses, stop. No exceptions.         |
| Exit model         | Full TP at 2R   | No partials, no moving TP, SL stays          |
| SL type            | Market          | Limit SL can gap through → challenge breach  |
| Min confluence     | 3 factors       | Below 3 = not a trade, no matter how it looks|

---

## The Only Setup: Range VWAP Reversion (Start Here)

Add setups ONLY after 50+ trades with this one reviewed.

### Entry Checklist (ALL boxes must be checked)

```
[ ] 1. REGIME: Price is ranging (no clear HH/HL or LH/LL on 4H)
[ ] 2. LOCATION: Price is at range extreme (bottom 20% or top 20%)
[ ] 3. VWAP: Price is beyond VWAP ±1.5σ band
[ ] 4. CONFLUENCE: Scanner shows ≥3 aligned factors
[ ] 5. TRIGGER: 15m candle has CLOSED with rejection (wick > body)
         - NOT "about to close" — CLOSED
[ ] 6. RSI: Below 35 (for long) or above 65 (for short) on 15m
[ ] 7. RISK: Position sized at exactly 0.5% risk
[ ] 8. SL/TP: Both calculated and ready BEFORE clicking
[ ] 9. DAILY CHECK: Haven't hit daily stop or 2 consecutive losses
[ ] 10. MINDSET: I am NOT trying to "make back" anything
```

**If ANY box fails → walk away. No "close enough."**

---

## Execution Procedure

### When Bot Sends Alert

1. **WAIT 60 seconds** (kills impulse, lets you verify)
2. Open chart. Confirm:
   - Setup still valid (price hasn't already moved to TP area)
   - No obvious news event in next 30 min
   - Candle trigger actually occurred (closed, not wicking)
3. **Place order** with pre-calculated:
   - Entry: limit at current price (or market if moving)
   - SL: market stop at scanner's SL level
   - TP: limit at scanner's TP1 (2R level)
4. **Set SL within 60 seconds** of entry (HyroTrader: 5 min max)
5. **Walk away.** Do not watch the trade.

### After Entry — DO NOT:

- Move your stop loss (it stays where it was placed)
- Move your take profit
- Add to the position
- "Check" the trade every 5 minutes
- Close early because "it looks like it's turning"

### The Trade Resolves When:

- **TP hits** → +2R → log it → wait for next alert
- **SL hits** → -1R → log it → check if daily limits hit
- **4 hours pass** (time-stop) → close at market → log as 0R

---

## Daily Routine

### Pre-Session (5 min)

1. Check account equity
2. Note daily high-water mark
3. Confirm: losses today = 0, trades today = 0
4. Scanner is running and receiving data
5. Mental state check: am I calm? If not → don't trade today

### During Session

- Only act when bot sends alert
- Follow execution procedure exactly
- After each trade: update the log (setup, R result, rules followed Y/N)

### Post-Session (5 min)

- Log final equity
- Note: trades taken, R gained/lost, rules followed?
- If daily stop hit: log out, done

### Weekly Review (Sunday, 30 min)

- Total R for the week
- Win rate (target: >45%)
- Average win / average loss (target: win > 1.5x loss)
- Rule compliance % (target: 100%)
- Trading days completed toward minimum 10
- ONE adjustment allowed (if backed by data from 20+ trades)

---

## Circuit Breakers (Automatic — Cannot Override)

| Condition                  | Action           | Reset             |
|---------------------------|------------------|-------------------|
| 2 consecutive losses      | Stop trading     | Next calendar day |
| Daily PnL ≤ -1.5R        | Stop trading     | Next calendar day |
| 3 trades placed today     | Stop trading     | Next calendar day |
| Daily drawdown ≥ 4%       | Flatten all      | Manual review     |
| Overall drawdown ≥ 6%     | Halt system      | Manual review     |
| Win rate < 30% over 20    | Pause, review    | After rule change |

---

## What Counts as a Trading Day (HyroTrader Rules)

For a day to count toward the 10-day minimum:

1. **Trade size ≥ 5% of initial balance** ($5,000 on $100K)
2. **PnL ≥ ±1% of trade value** ($50 on a $5,000 trade)
3. Day is counted by the **exit date** of the trade

At 0.5% risk and 2R target:
- Position size: ~$5,000–$15,000 (depends on SL distance)
- TP hit = +1R of trade value ≈ +1% to +3% → qualifies
- SL hit = -0.5R to -1R ≈ -0.5% to -1% → may qualify

**Every 2R win qualifies. Most 1R losses qualify. You're covered.**

---

## The Meta-Rule

> Your KPI is not profit. Your KPI is: **"Did I follow the process?"**
>
> A losing trade taken perfectly is a good trade.
> A winning trade taken emotionally is a bad trade.
>
> Execute the process 50 times. Then evaluate.
