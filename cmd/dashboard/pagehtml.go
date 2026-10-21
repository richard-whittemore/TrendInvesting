package main

// pageHTML is the whole page: one document with its styles inline, no
// script, no font file and no outside resource. The look is the api-style
// dashboard's structure (a header, a row of figures, the equity comparison
// first, dense tables) on rounded, softly shadowed cards in the system font
// stack. Colours are variables: light by default, dark when the device asks
// for it, and either when the reader's ?theme= link says so.
const pageHTML = `{{define "needtwo"}}<p class="note">The chart needs two paired points (a date with both an account snapshot and a benchmark close) and has fewer.</p>{{end}}
{{define "equity"}}{{if .Drawn}}<svg class="chart" role="img" viewBox="0 0 480 216" aria-label="{{.Label}}">
{{range .Ticks}}<path class="grid" d="M{{$.Left}} {{.Y}}H{{$.Right}}"/><text class="axis" x="{{$.LabelX}}" y="{{.TextY}}" text-anchor="end">{{.Text}}</text>{{end}}
<path class="base" d="M{{.Left}} {{.BaseY}}H{{.Right}}"/>
<path class="line-benchmark" d="{{.BenchmarkPath}}"/><path class="line-account" d="{{.AccountPath}}"/>
<rect class="mark-benchmark" x="{{.BenchmarkFirst.SX}}" y="{{.BenchmarkFirst.SY}}" width="8" height="8"/><rect class="mark-benchmark" x="{{.BenchmarkLast.SX}}" y="{{.BenchmarkLast.SY}}" width="8" height="8"/>
<circle class="mark-account" cx="{{.AccountFirst.CX}}" cy="{{.AccountFirst.CY}}" r="4.5"/><circle class="mark-account" cx="{{.AccountLast.CX}}" cy="{{.AccountLast.CY}}" r="4.5"/>
<text class="end-account" x="{{.EndX}}" y="{{.AccountEndY}}">{{.AccountEnd}}</text><text class="end-benchmark" x="{{.EndX}}" y="{{.BenchmarkEndY}}">{{.BenchmarkEnd}}</text>
<text class="date" x="{{.Left}}" y="204">{{.StartDate}}</text><text class="date" x="{{.Right}}" y="204" text-anchor="end">{{.EndDate}}</text></svg>
<p class="legend"><span><svg width="28" height="12" viewBox="0 0 28 12" aria-hidden="true"><path class="key-line-account" d="M1 6H27"/><circle class="key-mark-account" cx="14" cy="6" r="4"/></svg>Account, solid line, circles</span><span><svg width="28" height="12" viewBox="0 0 28 12" aria-hidden="true"><path class="key-line-benchmark" d="M1 6H27"/><rect class="key-mark-benchmark" x="10" y="2" width="8" height="8"/></svg>{{.Benchmark}}, dashed line, squares</span></p>{{else}}{{template "needtwo"}}{{end}}{{end}}
{{define "strip"}}{{if .Drawn}}<svg class="chart" role="img" viewBox="0 0 480 216" aria-label="{{.Label}}">
<path class="grid" d="M{{.Left}} {{.TopY}}H{{.Right}}"/><path class="grid" d="M{{.Left}} {{.BottomY}}H{{.Right}}"/><path class="base" d="M{{.Left}} {{.MidY}}H{{.Right}}"/>
<text class="axis" x="{{.LabelX}}" y="{{.TopText}}" text-anchor="end">{{.TopLabel}}</text><text class="axis" x="{{.LabelX}}" y="{{.MidText}}" text-anchor="end">0</text><text class="axis" x="{{.LabelX}}" y="{{.BotText}}" text-anchor="end">{{.BottomLabel}}</text>
{{range .Bars}}<rect class="bar-account" x="{{.AX}}" y="{{.AY}}" width="{{$.Width}}" height="{{.AH}}"/><rect class="bar-benchmark" x="{{.BX}}" y="{{.BY}}" width="{{$.Width}}" height="{{.BH}}"/>{{end}}
<text class="date" x="{{.Left}}" y="204">{{.StartDate}}</text><text class="date" x="{{.Right}}" y="204" text-anchor="end">{{.EndDate}}</text></svg>
<p class="legend"><span><svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true"><rect class="key-bar-account" x="1" y="1" width="10" height="10"/></svg>Account, left bar of each pair</span><span><svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true"><rect class="key-bar-benchmark" x="1" y="1" width="10" height="10"/></svg>{{.Benchmark}}, right bar</span></p>
<p class="legend">Each bar is the change from one paired point to the next, which can be several sessions apart when a snapshot or a SPY close is missing; points are spaced evenly, not by calendar. Shows the most recent {{.Cap}} changes at most.</p>{{else}}{{template "needtwo"}}{{end}}{{end}}
<!doctype html>
<html lang="en"{{if .Theme}} data-theme="{{.Theme}}"{{end}}><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Trend Investing</title>
<style>
:root,:root[data-theme="light"]{color-scheme:light;--bg:#f2f2f7;--card:#fff;--text:#000;--muted:#5c5c61;--line:#d1d1d6;--chip:#e5e5ea;--acct:#6b3fc4;--bench:#48484a;--pos:#1b7a34;--neg:#c4001a;--warn:#9a4a00;--link:#0060c0;--shadow:0 1px 2px rgba(0,0,0,.06),0 4px 14px rgba(0,0,0,.06)}
@media(prefers-color-scheme:dark){:root:not([data-theme="light"]){color-scheme:dark;--bg:#000;--card:#1c1c1e;--text:#fff;--muted:#98989e;--line:#38383a;--chip:#2c2c2e;--acct:#b08cf5;--bench:#d1d1d6;--pos:#30d158;--neg:#ff6961;--warn:#ff9f0a;--link:#409cff;--shadow:none}}
:root[data-theme="dark"]{color-scheme:dark;--bg:#000;--card:#1c1c1e;--text:#fff;--muted:#98989e;--line:#38383a;--chip:#2c2c2e;--acct:#b08cf5;--bench:#d1d1d6;--pos:#30d158;--neg:#ff6961;--warn:#ff9f0a;--link:#409cff;--shadow:none}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:15px/1.4 -apple-system,BlinkMacSystemFont,"SF Pro Text","SF Pro Display",system-ui,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased}
a{color:var(--link)}a:focus-visible{outline:3px solid var(--link);outline-offset:2px;border-radius:8px}
code,time,.mono,td{font-variant-numeric:tabular-nums}
.mono{font-family:ui-monospace,"SF Mono",Menlo,Consolas,monospace}
header.top{position:sticky;top:0;z-index:2;display:flex;flex-wrap:wrap;align-items:center;gap:.5rem 1rem;padding:.5rem 1rem;background:var(--card);border-bottom:.5px solid var(--line)}
header.top .brand{font-weight:600;display:flex;align-items:center;gap:.5rem}
header.top .logo{display:inline-flex;width:1.75rem;height:1.75rem;border-radius:8px;background:var(--acct);align-items:center;justify-content:center}
.pill{display:inline-flex;align-items:center;gap:.375rem;padding:.125rem .5rem;border-radius:6px;background:var(--chip);color:var(--muted);font-size:13px;font-weight:600}
.theme{margin-left:auto;display:flex;align-items:center;gap:.5rem;font-size:13px;color:var(--muted)}
.theme span.seg{display:inline-flex;padding:2px;border-radius:9px;background:var(--chip)}
.theme a{display:inline-flex;align-items:center;justify-content:center;min-width:4.5rem;min-height:2.75rem;padding:0 .75rem;border-radius:7px;color:var(--text);text-decoration:none}
.theme a[aria-current]{background:var(--card);font-weight:600;box-shadow:var(--shadow)}
.wrap{display:flex;flex-wrap:wrap;gap:1.75rem;align-items:flex-start;max-width:80rem;margin:0 auto;padding:1.25rem 1rem 3rem}
aside{flex:0 1 14rem;min-width:0;display:flex;flex-direction:column;gap:.5rem}
aside h2,.eyebrow{margin:0;padding:0 .25rem;font-size:13px;font-weight:400;text-transform:uppercase;letter-spacing:.02em;color:var(--muted)}
aside nav,aside .links{display:flex;flex-direction:column;background:var(--card);border-radius:16px;box-shadow:var(--shadow);overflow:hidden}
aside nav a,aside .links a{display:flex;align-items:center;gap:.75rem;min-height:2.75rem;padding:0 1rem;color:var(--text);text-decoration:none;border-top:.5px solid var(--line)}
aside nav a:first-child,aside .links a:first-child{border-top:0}
aside .links a{color:var(--link);font-size:13px}
aside p{margin:0;font-size:12px;color:var(--muted)}
main{flex:1 1 34rem;min-width:0;display:flex;flex-direction:column;gap:1.75rem}
main h1{margin:0;font-size:28px;line-height:1.15}
.sub{margin:.25rem 0 0;color:var(--muted)}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(14rem,1fr));gap:.75rem}
.tile{display:flex;flex-direction:column;gap:.25rem;padding:1rem 1.25rem;background:var(--card);border-radius:16px;box-shadow:var(--shadow);color:var(--text);text-decoration:none}
.tile .big,.stat .big{font-size:28px;font-weight:700;letter-spacing:-.01em;font-variant-numeric:tabular-nums}
.tile .row{display:flex;justify-content:space-between;gap:.5rem;font-size:13px;color:var(--muted)}
.dot{display:inline-block;width:.5rem;height:.5rem;margin-right:.375rem;border-radius:50%;background:currentColor}
.dot.bad{border-radius:1px;transform:rotate(45deg)}
.ok{color:var(--pos)}.warn{color:var(--warn)}.bad{color:var(--neg)}.pos{color:var(--pos)}.neg{color:var(--neg)}
section.acct{display:flex;flex-direction:column;gap:1rem;scroll-margin-top:5rem}
section.acct>header{display:flex;flex-wrap:wrap;align-items:center;gap:.5rem .75rem}
section.acct h2{margin:0;font-size:22px}
section.acct h2 small{font-size:13px;font-weight:400;color:var(--muted)}
.cols{display:flex;flex-wrap:wrap;gap:1rem;align-items:flex-start}
.cols>div{flex:1 1 22rem;min-width:0;display:flex;flex-direction:column;gap:1rem}
.card{background:var(--card);border-radius:16px;box-shadow:var(--shadow);padding:1rem 1.25rem;overflow-x:auto}
.card h3{margin:0 0 .5rem;font-size:17px}
.card p{margin:.4rem 0}
.card table{width:100%;border-collapse:collapse}
td,th{padding:.35rem .6rem;text-align:left;border-bottom:.5px solid var(--line)}
tr:last-child td{border-bottom:0}
.chips{display:flex;flex-wrap:wrap;gap:.5rem;margin:.25rem 0}
.chip{display:inline-flex;align-items:center;gap:.25rem;padding:.125rem .5rem;border-radius:6px;background:var(--chip);font-size:13px;font-weight:600}
.chart{display:block;width:100%;max-width:34rem;height:auto}
.chart .grid{stroke:var(--line);stroke-width:1;fill:none}
.chart .base{stroke:var(--muted);stroke-width:1;fill:none}
.chart text{font-size:12px;fill:var(--muted)}
.chart text.date{font-family:ui-monospace,"SF Mono",Menlo,Consolas,monospace}
.chart text.end-account{fill:var(--acct);font-weight:600}
.chart text.end-benchmark{fill:var(--bench);font-weight:600}
.line-account,.key-line-account{fill:none;stroke:var(--acct);stroke-width:2.5;stroke-linejoin:round}
.line-benchmark,.key-line-benchmark{fill:none;stroke:var(--bench);stroke-width:2;stroke-dasharray:6 4;stroke-linejoin:round}
.mark-account,.key-mark-account,.bar-account,.key-bar-account{fill:var(--acct)}
.mark-benchmark,.key-mark-benchmark,.bar-benchmark,.key-bar-benchmark{fill:var(--bench)}
.legend{display:flex;flex-wrap:wrap;gap:.25rem 1rem;margin:.25rem 0;font-size:13px;color:var(--muted)}
.legend span{display:inline-flex;align-items:center;gap:.375rem}
.note,.assume{color:var(--muted);font-size:13px}
.pairs{display:grid;grid-template-columns:auto 1fr;gap:.25rem 1rem;font-size:13px}
.pairs dt{color:var(--muted)}.pairs dd{margin:0}
.urgent{font-weight:700}
tr.urgent td:first-child{border-left:3px solid var(--neg)}
@media(max-width:40rem){.theme{margin-left:0}}
</style></head>
<body>
<header class="top">
<span class="brand"><span class="logo" aria-hidden="true"><svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="#fff" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12l4-4 3 3 5-6"/></svg></span>Trend Investing</span>
<span class="pill">Read-only</span>
<nav class="theme" aria-label="Theme"><span>Theme</span><span class="seg">
<a href="?theme=light"{{if eq .Theme "light"}} aria-current="true"{{end}}>Light</a>
<a href="?theme=dark"{{if eq .Theme "dark"}} aria-current="true"{{end}}>Dark</a></span></nav>
</header>
<div class="wrap">
<aside>
<h2>Accounts</h2>
<nav aria-label="Accounts">{{range .Accounts}}<a href="#acct-{{.Summary.AccountID}}"><span class="mono">{{.Summary.AccountID}}</span><span class="{{stateClass .Summary.EngineState}}" style="margin-left:auto;font-size:13px"><span class="dot {{stateClass .Summary.EngineState}}" aria-hidden="true"></span>{{.Summary.EngineState}}</span></a>{{end}}</nav>
<h2>Pages</h2>
<div class="links"><a href="/api/summary.json">/api/summary.json</a><a href="/healthz">/healthz</a></div>
<p>GET and HEAD only. Anything else answers 405 &ldquo;read-only dashboard&rdquo;. Without a theme choice the page follows this device&rsquo;s setting; the page runs no scripts, and every request re-reads and verifies each current journal; an earlier run&rsquo;s log is read again only when it changes.</p>
</aside>
<main>
<div><h1>Trend Investing (read-only)</h1><p class="sub">The latest day each journal holds, with every alert&rsquo;s full text.</p></div>
<div class="tiles">{{range .Accounts}}{{with .Summary}}<a class="tile" href="#acct-{{.AccountID}}"><span class="row"><span class="mono">{{.AccountID}} &middot; {{.StrategyID}}</span><span class="{{stateClass .EngineState}}"><span class="dot {{stateClass .EngineState}}" aria-hidden="true"></span>{{.EngineState}}</span></span>{{if .HaveSnapshot}}<span class="big">{{money .NetLiquidation}}</span><span class="row">Net liquidation</span>{{else}}<span class="row">No account snapshot.</span>{{end}}</a>{{end}}{{end}}</div>
{{range .Accounts}}{{with .Summary}}<section class="acct" id="acct-{{.AccountID}}">
<header><h2>{{.AccountID}} <small>{{.StrategyID}}, {{.Date}}, engine {{.EngineState}}</small></h2></header>
<div class="cols"><div>
<div class="card">
{{if .HaveSnapshot}}<p>Net liquidation {{printf "%.2f" .NetLiquidation}} as of {{.HoldingsAsOf.UTC.Format "2006-01-02 15:04Z"}}</p>{{else}}<p>No account snapshot.</p>{{end}}
<h3>Equity against {{.Benchmark}}</h3>{{if .Equity.HistoryUnavailable}}<p class="warn">{{.Equity.HistoryUnavailable}}.</p><p class="assume">The comparison here starts at {{.Equity.HistoryStartsAt}}, the first intact run: the runs before it are left out until the chain is whole.</p>{{end}}{{if .Equity.Available}}<div class="chips"><span class="chip {{tone .Equity.AccountReturn}}">{{arrow .Equity.AccountReturn}} {{pct .Equity.AccountReturn}} account</span><span class="chip {{tone .Equity.BenchmarkReturn}}">{{arrow .Equity.BenchmarkReturn}} {{pct .Equity.BenchmarkReturn}} {{.Benchmark}}</span><span class="chip {{tone .Equity.Excess}}">{{arrow .Equity.Excess}} {{pct .Equity.Excess}} difference</span></div>
<p>{{.Equity.StartDate}} to {{.Equity.EndDate}}: account {{pct .Equity.AccountReturn}}, {{.Benchmark}} {{pct .Equity.BenchmarkReturn}}, difference {{pct .Equity.Excess}}.<br>{{if .Equity.DayAvailable}}Day, {{.Equity.DayStartDate}} to {{.Equity.EndDate}}: account {{pct .Equity.DayAccountReturn}}, {{.Benchmark}} {{pct .Equity.DayBenchmarkReturn}}.{{else}}Day: not available (no paired snapshot and {{.Benchmark}} close on {{.Date}}).{{end}}</p>
{{template "equity" chart .}}
<p class="assume">{{count .Equity}} paired points, {{.Equity.StartDate}} to {{.Equity.EndDate}}. {{assumption}}</p>
{{if gt (count .Equity) 1}}<dl class="pairs"><dt>First paired point</dt><dd class="mono">{{(first .Equity).Date}} &middot; {{money (first .Equity).Net}} &middot; {{.Benchmark}} {{money (first .Equity).Benchmark}}</dd>
<dt>Before</dt><dd class="mono">{{(before .Equity).Date}} &middot; {{money (before .Equity).Net}} &middot; {{.Benchmark}} {{money (before .Equity).Benchmark}}</dd>
<dt>Latest paired point</dt><dd class="mono">{{(last .Equity).Date}} &middot; {{money (last .Equity).Net}} &middot; {{.Benchmark}} {{money (last .Equity).Benchmark}}</dd></dl>
<p class="assume">The same starting balance held in {{.Benchmark}} would be {{held .Equity}} on {{.Equity.EndDate}}.</p>{{end}}
{{else}}<p>Not enough paired snapshots and bars yet.</p>{{template "equity" chart .}}{{end}}
</div>
<div class="card"><h3>Change between paired points</h3>{{template "strip" strip .}}</div>
</div><div>
<div class="card"><h3>Holdings</h3>{{if .Holdings}}<table>{{range .Holdings}}<tr><td>{{.InstrumentID}}</td><td>{{.Quantity}}</td></tr>{{end}}</table>{{else}}<p>None.</p>{{end}}</div>
<div class="card"><h3>Targets</h3>{{if .Decisions}}<table>{{range .Decisions}}<tr><td>{{.Outcome}}</td><td>{{.WithheldReason}}</td><td>{{range .Targets}}{{.InstrumentID}} {{weight .Weight}} {{end}}</td></tr>{{end}}</table>{{else}}<p>No decision today.</p>{{end}}</div>
<div class="card"><h3>Orders</h3>{{if .Orders}}<table>{{range .Orders}}<tr><td>{{.ClientOrderID}}</td><td>{{.Side}} {{.Quantity}} {{.InstrumentID}} @ {{printf "%.2f" .LimitPrice}}</td><td>{{.StatusText}}</td><td>{{.Reason}}</td></tr>{{end}}</table>{{else}}<p>None.</p>{{end}}
<p>Refusals today: {{.Refusals}}</p></div>
<div class="card"><h3>Fills</h3>{{if .Fills}}<table>{{range .Fills}}<tr><td>{{.Side}}</td><td>{{.InstrumentID}}</td><td>{{.Quantity}} @ {{printf "%.2f" .Price}}</td><td>{{.ClientOrderID}}</td></tr>{{end}}</table>{{else}}<p>None.</p>{{end}}</div>
{{if .SalesWithheld}}<div class="card"><h3>Sales back to 98% withheld</h3><table>{{range .SalesWithheld}}<tr><td>{{.ClientOrderID}}</td><td>{{.InstrumentID}}</td><td>{{.Reason}}</td><td>{{.Trigger}}</td><td>{{.Detail}}</td></tr>{{end}}</table></div>{{end}}
{{if .Skipped}}<div class="card"><h3>Trades skipped</h3><table>{{range .Skipped}}<tr><td>{{.ClientOrderID}}</td><td>{{.Side}}</td><td>{{.InstrumentID}}</td><td>{{shares .Shares}} shares</td><td>{{.Reason}}</td></tr>{{end}}</table></div>{{end}}
{{if .RejectedInputs}}<div class="card"><p class="urgent">Inputs not read: {{.RejectedInputs}} (an unknown schema version or values that fail validation; they move no figure).</p></div>{{end}}
</div></div>
{{end}}<div class="card"><h3>Alerts</h3>{{if .Alerts}}<table>{{range .Alerts}}<tr class="{{.Severity}}"><td>{{.At.UTC.Format "2006-01-02 15:04Z"}}</td><td>{{.Severity}}</td><td>{{.Title}}<p>{{.Body}}</p></td></tr>{{end}}</table>{{else}}<p>None.</p>{{end}}</div>
</section>
{{end}}</main></div></body></html>
`
