"""Check OpenAPI YAML structure and resolve every internal reference (not full schema validation)."""
from pathlib import Path
import re
import yaml

spec = yaml.safe_load(Path("api/openapi.yaml").read_text())
assert spec["openapi"] == "3.1.0"
assert spec["paths"] and spec["components"]["schemas"]


def resolve(ref):
    assert ref.startswith("#/"), "External references need explicit review"
    target = spec
    for part in ref[2:].split("/"):
        target = target[part.replace("~1", "/").replace("~0", "~")]
    return target


def check(node):
    if isinstance(node, dict):
        if "$ref" in node:
            resolve(node["$ref"])
        for value in node.values():
            check(value)
    elif isinstance(node, list):
        for value in node:
            check(value)


check(spec)
for path, item in spec["paths"].items():
    for method, operation in item.items():
        if method not in {"get", "post", "put", "patch", "delete", "head", "options"}:
            continue
        assert operation["responses"], f"No responses: {method} {path}"
        parameters = item.get("parameters", []) + operation.get("parameters", [])
        parameters = [resolve(p["$ref"]) if "$ref" in p else p for p in parameters]
        for name in re.findall(r"\{([^}]+)\}", path):
            assert any(p.get("name") == name and p.get("in") == "path" and p.get("required") for p in parameters)
        for requirement in operation.get("security", spec.get("security", [])):
            for scheme in requirement:
                assert scheme in spec["components"]["securitySchemes"]
print("OpenAPI YAML, internal references, path parameters and security references passed")
