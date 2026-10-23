# Benchmark Subsystem — Design Input (Research Synthesis)

> [简体中文](../benchmark-research.md) | English

> 2026-09-01, a four-way research synthesis: SWE-bench family / τ-bench (pass^k) / METR methodology / promptfoo·LangSmith·Braintrust (engineering forms), plus confirmation of the Go ecosystem gap. Positioning: a **general-purpose benchmark library** — it evaluates any agent built with GoAgent; business-specific parts stay outside, at the pluggable boundary. This is the metrics-design input written before implementation, not an implementation plan.

## 0. A prior judgment: a benchmark is two systems, not one

| | Engine regression tests | Capability evaluation |
|---|---|---|
| Subject under test | loop/tool/pipeline behavior | agent task completion |
| Provider | mock, deterministic | real models, stochastic |
| Scoring | exact assertions | rules + judge + sampling |
| Cost | free, full runs anytime | real money |
| Noise | none | ±2× (METR, measured) |

Mixing them is the common mistake: using tier-2 cost and noise to do tier-1's job, or using tier-1's deterministic illusion to score tier 2. The API must let each tier stand on its own (same Runner, different Target/Judge combinations).

## 1. Conclusions independently converged by all four sources (the most trustworthy part)

These points were independently validated by two or more unrelated systems; they are the foundation of the whole design:

### 1.1 Repeated runs are not optional — they are the foundation of metrics

- METR: error bars of **±2×** — "agonizing over whether it's 10% or 20% is pointless"; variance at T=0 is also large; 3–6 independent runs per task is the floor
- τ-bench: pass/fail flipping across trials of the same task is the norm (gpt-4o retail pass^1 ≈ 60%, pass^8 drops to ~25%); the official leaderboard requires "4+ trials"
- promptfoo/LangSmith/Braintrust: repeat / num_repetitions built in across the board

**Design inference**: the Runner natively supports `Repeat`; reports present per-case variance rather than just means; `pass^k` is a first-class metric (`C(c,k)/C(N,k)` — k-for-k consistency, measuring reliability; pass@k measures the capability ceiling — both are needed, semantics kept separate).

### 1.2 Deltas are first-class; single scores are meaningless

- METR: trends are robust to 10× absolute measurement error; single points are incomparable; **relative comparison within the same suite has lower error** (performance correlation) — the quantitative basis for "regression diff first"
- SWE-bench: the leaderboard caches by run_id+instance_id; changing a patch requires a new run_id
- promptfoo: multi-run comparison / `filterFailing` re-runs only failures

**Design inference**: only deltas exceeding CI count as real signal; Reports land as JSONL; `Diff(a, b)` emits a state-transition list (pass→fail and so on); **cache keys must include an input content hash** (the SWE-bench trap: keys without the prediction serve stale results when patches change).

### 1.3 Scored verdicts, not booleans

All four engineering tools return `Verdict{Pass, Score 0-1, Reason, Metric}` rather than bool. Weighting, aggregation, pass^k, and variance statistics all need continuous scores. **Design inference**: assertions return structured Verdicts; bool is syntax sugar for Score > threshold.

### 1.4 Three-way failure classification; infra flakiness stays out of the denominator

- τ-bench: infra errors excluded explicitly
- SWE-bench: infra_failure/ambiguous secondary classification added late (after running with the disease for a long time)

**Design inference**: the result state machine is four-state from day one — `pass / fail / infra_error / excluded`. infra_error auto-retries and stays out of regression diffs — otherwise infrastructure flakiness drowns real regressions.

## 2. Assertion system (the densest input from the four sources)

### 2.1 Two channels × two assertion sets (τ-bench × SWE-bench crossover)

- **τ-bench**: reward = product of components, `reward_basis = [DB, COMMUNICATE]` — whether the conversation was right (did it say what should be said) and whether the side effects were right (final state) are independent channels
- **SWE-bench**: `FAIL_TO_PASS` (not satisfied at baseline, satisfied after success — guards against false positives) × `PASS_TO_PASS` (held before and after — guards against collateral breakage) — the verdict is the AND

**Design inference**: at least four assertion classes:
1. `DialogueAssertion`: output content (contains/regex/is-json/llm-judge)
2. `StateAssertion`: final-state assertions (tool side effects, filesystem, task store state)
3. `TrajectoryAssertion`: tool-call sequence/parameters (**our differentiator** — promptfoo needs bolt-on tracing for trajectory assertions; our observer has them natively)
4. `PreserveAssertion`: P2P equivalence — things that shouldn't have changed didn't

### 2.2 Derive expected end-states by replay, don't hand-write them (the SABER lesson)

τ-bench was systematically audited at 50+ labeling errors; after the fix, airline rose 14–20 points — meaning the original had long been judging "correct refusals" as wrong. Hand-written expected states are a labeling-error minefield. **Design inference**: provide GoldReplayer — replay the case's reference tool-call sequence in a resettable mock environment to derive the expected end-state snapshot; compare only final states, never paths (all equivalent paths pass).

### 2.3 Validate cases before they enter the suite (the SWE-bench validation stage)

Every case runs dual-ended validation before admission: the gold trajectory scores 1 and a deliberately wrong trajectory scores 0 (τ-bench research note #8). **A case that fails this is a broken case and must not enter the suite** — this moves "the grader itself has a bug" from runtime pollution to an admission-time block.

### 2.4 Assertion library shape (the promptfoo template)

- 8–10 built-in deterministic primitives: contains / regex / is-json(+schema) / levenshtein / latency / cost / token-usage / tool-used / tool-sequence / sandbox-exec (compile and run the artifact in a sandbox — a capability no other library has)
- `RegisterAssert("name", fn)` as the light escape hatch — **the negative lesson of OpenAI evals**: custom evals requiring class inheritance and two methods got flamed for three years; the extension point must be a first-class escape hatch, not a heavy plugin system
- Judge as a separate interface: `Judge(ctx, Case, Result) Verdict`; the built-in LLMJudge reuses the library's own provider, with rubric templates taken from promptfoo/OpenAI's validated factuality / closedqa / select-best

## 3. Library vs host boundary (the core line for the general-purpose positioning)

All four engineering tools converge on the same line — adopted directly:

| Built into the library | Host-defined |
|---|---|
| Case/Assert/Judge/Target/Report types and loading | The golden data itself |
| Deterministic assertion primitives + registry | Domain scoring logic (RegisterAssert) |
| LLM judge templates (factuality etc.) | Rubric text |
| Matrix runs (multiple targets × cases) | Assembly of the agent under test (Target implementation) |
| Repeat/variance/pass^k/CI metrics | Manual sampling process |
| JSONL reports + Diff + junit.xml | The task's domain semantics |

Go ecosystem confirmed: langchaingo (1492 paths) and eino + eino-ext (1605 paths) **have no eval module** — GoAgent fills a genuine vacuum, not a reinvented wheel.

## 4. Anti-self-deception defenses for self-improvement (README's three + research hardening)

README already has: benchmark edits are append-only, held-out sets invisible, manual sampling cannot be fully removed. The research adds four harder ones:

1. **Answer leakage is a physical problem** (SWE-bench warning): storing cases where the agent can read them = assertion leakage; the agent can literally look up the answers. Assertion fields default to **not injected into agent context**; case files provide a hidden packed form (assertions stored separately); sandboxed case graders verify the agent never read the answer files
2. **Audit successful runs for cheating too** (METR states explicitly that "only checking failures is a biased process"): when scores rise, first rule out "learned to fool the judge". LLM monitors only flag; confirmation goes through an independent channel; confirmed cheating is re-scored as failure
3. **Algorithmic grading systematically overestimates** (METR: PRs with a 38% test pass rate dropped to 0% mergeable after human review): hill-climbing on a gameable score amplifies reward hacking without real gains. Use "could I ship this directly"-style rubrics for sampling
4. **Pre-registered exclusion rules**: only fixed categories (broken task / missing tool / ambiguity / cheating / contamination) may be excluded, with logging — preventing "delete the hard ones to inflate the score"

## 5. Other directly copyable engineering details

- Task set **version + content hash** written into reports (tau3: after scoring changes, old and new results are incomparable)
- Reports carry a per-case artifact directory (log/output/trace references); diffs list only state transitions
- Fixed temperature + seed is the consensus baseline but doesn't eliminate randomness — "low temperature + repeats + variance presentation" is the standard posture
- Task family structure: family + ≥4 parameterized variants; analysis granularity at family level (METR's "bottleneck diversity" argument); difficulty spread from seconds to minutes to avoid saturation
- Metrics package kept independent: PassHatK / PassAtK / mean+variance / bootstrap CI are pure functions over (trials, successes), not the runtime

## 6. Phased delivery

- **B0 (minimal loop, pure mock)**: Case/Assert types + deterministic assertion primitives + Runner (Repeat/four-state results) + Report (JSONL) + Diff + dogfooding (benchmarking GoAgent's own file tools — engine regression tier first)
- **B1 (real-model tier)**: Judge interface + LLMJudge + pass^k metrics + variance/CI presentation + junit.xml; the null experiment happens at this tier (below)
- **B2 (self-improvement armament)**: GoldReplayer + StateAssertion + hidden assertion separation + cheating-flag pipeline + Issue integration (regression → IssueReport → fix → re-run)
- **B3 (long-term)**: user simulator (τ-bench style, built on MockProvider), parameterized task-family generator

**Where the null experiment sits**: immediately after B1 — pick 3–5 real agent tasks × 10 repeats and measure the score-distribution width. This data determines CI thresholds, default Repeat counts, and the default for "how large a delta is real signal". Without it, every later threshold is a guess.

### Null-experiment how-to (all tools exist at B1)

The null experiment: **change nothing**, run the same configuration twice, and see how much scores differ. That difference is the noise floor — any "improvement" smaller than it is noise.

```go
// 1. Real providers (use a different vendor for the judge to reduce same-source bias)
agentP := openai.New(...)
judgeP := anthropic.New(...)

target := goagent.NewAgentTarget("baseline", goagent.WithProvider(agentP))
run := func() *goagent.Report {
    r := &goagent.Runner{
        Targets: []goagent.Target{target},
        Cases:   suite,          // 3-5 real tasks, each with 2-3 rubric lines
        Repeat:  10,             // τ-bench convention: 4 is the floor, 10 to see the distribution
        Judge:   &goagent.LLMJudge{Provider: judgeP},
    }
    rep, _ := r.Run(ctx)
    return rep
}

a, b := run(), run()                       // twice, changing nothing in between
d := goagent.DiffReports(a, b)             // how much unstable beyond zero transitions?
for _, s := range a.Summaries() {
    // look at ScoreStdDev / ScoreCI95: how large is per-case variance
}
for _, pk := range a.PassK(2) {            // how far pass^2 falls from the pass rate
}
```

How to read it (METR's baseline discipline):

| Observation | Healthy | Noise alarm |
|---|---|---|
| ScoreStdDev over 10 trials of one case | < 0.15 | > 0.25 → double Repeat or tighten the rubric |
| Diff between two runs | zero transitions | regressed>0 → thresholds must exceed that noise |
| pass^2 / pass rate ratio | > 0.7 | < 0.4 → a reliability problem, not an evaluation problem |
| judge scores vs 20% manual sampling | agreement > 85% | lower → recalibrate the judge prompt or switch models |

Conclude by writing the measured noise floor into CI config (e.g. "MeanScore delta < 2× noise floor is not a regression") and backfilling Runner.Repeat / Timeout default recommendations. Run this once before every new case suite ships, and re-run quarterly.

## 7. Known points of divergence (decide when needed)

- METR's "grading always algorithmic, LLM only pre-screens" vs promptfoo's "llm-rubric is the workhorse assertion" — really a difference of scenario: "capability evaluation" vs "prompt tuning". Our two-tier system takes from each: the regression tier is purely algorithmic; the capability tier is judge-led with manual sampling as backstop
- SWE-bench freezes release sets vs continuously harvesting new cases — we are a self-improvement tool, not an academic leaderboard, and explicitly choose **no freezing, harvestable**: cases carry created_at/difficulty metadata and support retirement, but retirement follows pre-registered rules
