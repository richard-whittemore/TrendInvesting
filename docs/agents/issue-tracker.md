# Issue tracker: GitHub Issues (via the GitHub MCP)

Issues and specs for this repo live as **GitHub issues in `richard-whittemore/TrendInvesting`**.

**Access them only through the GitHub MCP tools.** The `gh` CLI is **not installed** on this machine — any instruction elsewhere to run `gh issue …` should be translated to the MCP equivalent below.

| Operation | Tool |
|---|---|
| Create / edit an issue | `mcp__github__issue_write` (method `create` / `update`) |
| Read an issue (with comments) | `mcp__github__issue_read` |
| List / search issues | `mcp__github__list_issues`, `mcp__github__search_issues` |
| Comment | `mcp__github__add_issue_comment` |
| Parent / child link | `mcp__github__sub_issue_write` |
| Labels | `mcp__github__issue_write` (`labels` field); `mcp__github__get_label` to check one exists |
| Close | `mcp__github__issue_write` with state `closed` **and** `state_reason` |
| PRs | `mcp__github__create_pull_request`, `pull_request_read`, `pull_request_review_write`, `merge_pull_request` |

Notes and limits:

- **The MCP cannot create labels, and it validates every label before writing.** Passing a label that does not already exist fails the whole call with `failed to resolve label "<name>"` — the issue is not created, so there is no partial state, but the write must be retried. `get_label` checks one label; there is no list-labels tool.
- **To create a label, use the REST API directly.** The environment carries `GITHUB_MCP_PAT`, the same credential the MCP uses. `POST /repos/{owner}/{repo}/labels` with `Authorization: Bearer $GITHUB_MCP_PAT` creates one (201), or returns 422 if it already exists. `POST /repos/{owner}/{repo}/issues/{n}/labels` adds labels to an existing issue without removing its current ones. Never echo the token; reference it only as an environment variable. Prefer the MCP for everything it supports — this is the documented exception, not a general licence to bypass it.
- The MCP exposes no native issue-dependency endpoint. Represent blocking edges as described under **Blocking** below.
- Paginate in batches of 5–10 and use `minimal_output` when the full body isn't needed.

## Ticket standard

Every ticket must be self-sufficient: an agent picking it up should need nothing beyond the ticket, `AGENTS.md`, `CONTEXT.md`, the ADRs it links, and the repo itself. Use these sections.

```markdown
## Why
The outcome or risk this addresses. Link the spec section, ADR, or `docs/methodology/` reference.

## What
The observable behaviour when this is done. State explicit exclusions.

## How
The approach, where it is not obvious or where it matters: data structures, invariants,
state machines, formulas with their source citation (e.g. "N per The Turtle Rules p.13").
Omit this section only when the approach is genuinely free choice.

## Tests expected
Named test cases to be written FIRST (TDD is mandatory — see AGENTS.md): golden fixtures to
transcribe, property invariants to assert, replay fixtures to produce, failure cases to cover.

## Related
- Blocked by: #N, #N
- Blocks: #N
- Context: #N (findings this ticket depends on)

## Work log
_(updated as work proceeds)_

## Findings
_(edited in place — the current answer, not a diary. The deliverable for research tickets.)_

## Testing evidence
Test names, `make check` result, coverage, replay-equivalence outcome.

## Concerns / new issues
Anything discovered that is out of scope. Each becomes a new linked issue before closing.
```

## Tickets are living records

- Update **Work log** as the work happens — what was done, how it was accomplished, how it was tested.
- Edit **Findings** in place so the top of the ticket is always the current truth; put the step-by-step narrative in **comments**.
- Record **concerns and newly discovered issues** on the ticket, then file each as its own linked issue.
- **Durability:** important findings must also land in a durable location — an ADR, `CONTEXT.md`, or a document under `docs/` — before the ticket closes. A ticket is a log; the repo is the record.

## Definition of done

Close a ticket only when **all** of the following hold, and say so in the closing comment:

1. Acceptance criteria in **What** are met.
2. **Testing evidence** is recorded and `make check` passes.
3. Durable findings are committed, with the commit or PR linked.
4. Every item under **Concerns** has been filed as its own issue.

Never close a ticket merely because code was written.

## Blocking

GitHub sub-issues are the canonical parent/child link (`mcp__github__sub_issue_write`). For blocking edges that are not parent/child, put a `- Blocked by: #N` list under **Related** at the top of the body. A ticket is unblocked when every issue in that list is closed. The **frontier** is every open, unassigned ticket whose blockers are all closed — those may be worked in parallel.

## Labels

- Triage roles: see `docs/agents/triage-labels.md`.
- **Implementer tier** (which model should pick this up): `tier/sonnet` (well-specified slice), `tier/opus` (design-heavy slice), `tier/codex` (cross-vendor second opinion).
- **Area** (optional): `area/methodology`, `area/go-core`, `area/lean`, `area/risk`, `area/data`, `area/ops`.

Until those labels exist in the repository, carry the same information as the first line of the issue body:

```
**Tier:** opus · **Area:** go-core · **Slice:** 1 · capital-safety
```

Once the labels are created, they can be applied to existing issues in a batch of `issue_write` updates.

## Pull requests as a triage surface

**PRs as a request surface: no.** _(Set to `yes` if this repo ever treats external PRs as feature requests; `/triage` reads this flag.)_

GitHub shares one number space across issues and PRs, so a bare `#42` may be either — resolve with `pull_request_read` and fall back to `issue_read`.

## When a skill says "publish to the issue tracker"

Create a GitHub issue with `mcp__github__issue_write`, using the ticket standard above.

## When a skill says "fetch the relevant ticket"

Read it with `mcp__github__issue_read`, including comments.

## Wayfinding operations

Used by `/wayfinder`. The **map** is a single issue labelled `wayfinder:map` holding Notes / Decisions-so-far / Fog; tickets are its sub-issues, labelled `wayfinder:<type>` (`research` / `prototype` / `grilling` / `task`). Blocking follows the **Blocking** section above. Claim by assigning yourself; resolve by commenting the answer, closing, and appending a pointer to the map's Decisions-so-far.
