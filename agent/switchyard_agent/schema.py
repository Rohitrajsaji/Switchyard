"""Versioned proposal documents. Extra fields are rejected so a model cannot smuggle policy."""

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

SCHEMA_VERSION = 1
SENSITIVE = {
    "email", "email_address", "phone", "phone_number", "name", "full_name",
    "address", "payment_id", "payment_identifier", "card_number",
}
MAX_INCREASE_BP = 1000


class FlagValue(BaseModel):
    model_config = ConfigDict(extra="forbid")
    type: Literal["boolean", "string", "number", "json"]
    data: bool | str | int | float | dict | list | None

    @model_validator(mode="after")
    def data_matches_type(self):
        if self.type == "boolean" and not isinstance(self.data, bool):
            raise ValueError("boolean data required")
        if self.type == "string" and (not isinstance(self.data, str) or len(self.data.encode()) > 1024):
            raise ValueError("string data required")
        if self.type == "number" and (isinstance(self.data, bool) or not isinstance(self.data, (int, float))):
            raise ValueError("number data required")
        if self.type == "json" and not isinstance(self.data, (dict, list, type(None))):
            raise ValueError("json data required")
        return self


class Rule(BaseModel):
    model_config = ConfigDict(extra="forbid")
    attribute: str
    operator: Literal["eq", "in", "gt", "gte", "lt", "lte"]
    values: list
    value: FlagValue

    @field_validator("attribute")
    @classmethod
    def attribute_allowed(cls, value: str) -> str:
        for part in value.lower().replace("-", "_").split("."):
            if part in SENSITIVE:
                raise ValueError("sensitive attribute")
        return value


class Rollout(BaseModel):
    model_config = ConfigDict(extra="forbid")
    traffic_bp: int = Field(ge=0, le=10000)
    value: FlagValue


class FlagContext(BaseModel):
    model_config = ConfigDict(extra="forbid")
    key: str
    type: Literal["boolean", "string", "number", "json"]
    revision: int
    killed: bool
    traffic_bp: int | None = None
    default: FlagValue
    safe: FlagValue
    rules: list[Rule] = Field(default_factory=list)
    rollout_value: FlagValue | None = None


class EnvironmentContext(BaseModel):
    model_config = ConfigDict(extra="forbid")
    project_id: str
    environment_id: str
    environment: str
    flags: list[FlagContext]


class ProposalDraft(BaseModel):
    """The JSON body posted to the agent proposal endpoint. schema_version is local only."""

    model_config = ConfigDict(extra="forbid")
    schema_version: Literal[1] = SCHEMA_VERSION
    environment_id: str
    key: str = Field(pattern=r"^[a-z][a-z0-9_-]{0,63}$")
    kind: Literal["create", "update"]
    type: Literal["boolean", "string", "number", "json"] | None = None
    expected_revision: int = Field(ge=0)
    default: FlagValue
    safe: FlagValue
    rules: list[Rule] = Field(default_factory=list)
    rollout: Rollout | None = None
    killed: bool = False
    rationale: str = Field(min_length=1, max_length=512)

    @model_validator(mode="after")
    def shape(self):
        if self.kind == "create" and (self.expected_revision != 0 or self.type is None):
            raise ValueError("create requires type and revision 0")
        if self.kind == "update" and self.expected_revision < 1:
            raise ValueError("update requires the current revision")
        return self

    def request_body(self) -> dict:
        body = self.model_dump(exclude={"schema_version"})
        return body

    def increase_bp(self, current: int | None) -> int:
        after = 0 if self.rollout is None else self.rollout.traffic_bp
        before = 0 if current is None else current
        return max(0, after - before)


def reject_oversized(draft: ProposalDraft, current_bp: int | None) -> None:
    if draft.increase_bp(current_bp) > MAX_INCREASE_BP:
        raise ValueError("rollout increase exceeds 10 percentage points")
