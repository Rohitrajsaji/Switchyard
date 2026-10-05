# ADR 0012: Agent proposals are untrusted input

## Status

Accepted for M11.

## Decision

A Python client can suggest a production flag change. The suggestion is ordinary JSON. Go validates it with the same code that validates a human proposal, then stores an exact diff. The application key may only read one environment's configuration and submit that proposal. The proposer is the human who created the key; the request body cannot name someone else. A different admin approves. Apply remains a human session action and is idempotent.

An agent key cannot also evaluate or write events, and it is refused by every direct mutation route. Standalone rollout increases from an agent are capped at 10 percentage points. Sensitive targeting attributes are rejected. The default provider is a deterministic mock. The hosted adapter does nothing unless a URL is configured, and the shipped client still refuses to send that call.

## Consequences

Human reviewers see the source as `agent` and the same before/after diff as a human proposal. A hosted model, if one is chosen later, cannot widen its own permissions. Disabling the key leaves human approval in place.
