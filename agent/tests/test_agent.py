import json

import pytest

from switchyard_agent.hosted import HostedProvider
from switchyard_agent.mock import MockProvider
from switchyard_agent.schema import EnvironmentContext, ProposalDraft, reject_oversized


def summary(flags):
    return EnvironmentContext.model_validate({
        "project_id": "prj_1",
        "environment_id": "env_prod",
        "environment": "production",
        "flags": flags,
    })


def listing(traffic):
    return {
        "key": "listing", "type": "boolean", "revision": 4, "killed": False, "traffic_bp": traffic,
        "default": {"type": "boolean", "data": False}, "safe": {"type": "boolean", "data": False},
        "rules": [], "rollout_value": {"type": "boolean", "data": True},
    }


def test_mock_raises_listing_by_ten_points():
    draft = MockProvider().suggest(summary([listing(1000)]))
    assert draft.kind == "update" and draft.expected_revision == 4
    assert draft.rollout.traffic_bp == 2000
    assert draft.increase_bp(1000) == 1000


def test_mock_creates_listing_when_absent():
    draft = MockProvider().suggest(summary([]))
    assert draft.kind == "create" and draft.rollout.traffic_bp == 1000 and draft.type == "boolean"


def test_sensitive_targeting_and_extra_fields_fail():
    with pytest.raises(Exception):
        ProposalDraft.model_validate({
            "environment_id": "env_prod", "key": "listing", "kind": "update", "expected_revision": 1,
            "default": {"type": "boolean", "data": False}, "safe": {"type": "boolean", "data": False},
            "rules": [{"attribute": "email", "operator": "eq", "values": ["a@b.test"], "value": {"type": "boolean", "data": True}}],
            "rationale": "target a person",
        })
    with pytest.raises(Exception):
        ProposalDraft.model_validate({
            "environment_id": "env_prod", "key": "listing", "kind": "update", "expected_revision": 1,
            "default": {"type": "boolean", "data": False}, "safe": {"type": "boolean", "data": False},
            "rationale": "smuggle", "role": "admin",
        })


def test_oversized_increase_is_rejected_locally():
    raw = {
        "environment_id": "env_prod", "key": "listing", "kind": "update", "expected_revision": 1,
        "default": {"type": "boolean", "data": False}, "safe": {"type": "boolean", "data": False},
        "rollout": {"traffic_bp": 5000, "value": {"type": "boolean", "data": True}},
        "rationale": "jump",
    }
    draft = ProposalDraft.model_validate(raw)
    with pytest.raises(ValueError):
        reject_oversized(draft, 1000)


def test_malformed_json_does_not_parse():
    with pytest.raises(Exception):
        ProposalDraft.model_validate_json("{")


def test_hosted_adapter_does_not_call_out():
    with pytest.raises(RuntimeError, match="no hosted model"):
        HostedProvider(None).suggest(summary([]))
    with pytest.raises(RuntimeError, match="not invoked"):
        HostedProvider("https://example.invalid/model").suggest(summary([]))


def test_request_body_has_no_schema_or_proposer():
    body = MockProvider().suggest(summary([listing(0)])).request_body()
    assert "schema_version" not in body and "proposer_id" not in body
    json.dumps(body)
