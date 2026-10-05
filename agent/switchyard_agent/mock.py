"""Deterministic suggestions. No network and no model call."""

from switchyard_agent.schema import (
    MAX_INCREASE_BP,
    EnvironmentContext,
    FlagValue,
    ProposalDraft,
    Rollout,
    reject_oversized,
)

FALSE = FlagValue(type="boolean", data=False)
TRUE = FlagValue(type="boolean", data=True)


class MockProvider:
    """Prefer a 10-point raise of an existing listing rollout; otherwise propose one at 10%."""

    def suggest(self, summary: EnvironmentContext) -> ProposalDraft:
        listing = next((flag for flag in summary.flags if flag.key == "listing"), None)
        if listing is not None and not listing.killed and listing.traffic_bp is not None and listing.traffic_bp <= 10000 - MAX_INCREASE_BP:
            draft = ProposalDraft(
                environment_id=summary.environment_id,
                key="listing",
                kind="update",
                expected_revision=listing.revision,
                default=listing.default,
                safe=listing.safe,
                rules=listing.rules,
                rollout=Rollout(traffic_bp=listing.traffic_bp + MAX_INCREASE_BP, value=listing.rollout_value or TRUE),
                killed=False,
                rationale="Raise listing rollout by 10 percentage points",
            )
            reject_oversized(draft, listing.traffic_bp)
            return draft
        draft = ProposalDraft(
            environment_id=summary.environment_id,
            key="listing",
            kind="create",
            type="boolean",
            expected_revision=0,
            default=FALSE,
            safe=FALSE,
            rollout=Rollout(traffic_bp=MAX_INCREASE_BP, value=TRUE),
            rationale="Start the listing rollout at 10 percent",
        )
        reject_oversized(draft, None)
        return draft
