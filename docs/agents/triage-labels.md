# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the actual label strings used in this repo's issue tracker.

| Label in mattpocock/skills | Label in our tracker | Meaning                                  |
| -------------------------- | -------------------- | ---------------------------------------- |
| `needs-triage`             | `needs-triage`       | Maintainer needs to evaluate this issue  |
| `needs-info`               | `needs-info`         | Waiting on reporter for more information |
| `ready-for-agent`          | `ready-for-agent`    | Fully specified, ready for an AFK agent  |
| `ready-for-human`          | `ready-for-human`    | Requires human implementation            |
| `wontfix`                  | `wontfix`            | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label string from this table.

These are the defaults, unchanged.

## Non-triage labels

Two other label groups exist and are **not** part of triage — see `docs/agents/issue-tracker.md`:

- `tier/sonnet` · `tier/opus` · `tier/codex` — which implementer should pick the ticket up.
- `area/*` — which part of the system the ticket touches.

The GitHub MCP cannot create labels. If one of these is missing, create it in the GitHub UI before applying it.
