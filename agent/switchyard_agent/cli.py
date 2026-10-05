"""Submit one mock proposal. Approval and apply stay with a different human."""

import argparse
import json
import os
import urllib.request

from switchyard_agent.hosted import HostedProvider
from switchyard_agent.mock import MockProvider
from switchyard_agent.schema import EnvironmentContext


def provider_from_env():
    name = os.environ.get("SWITCHYARD_AGENT_PROVIDER", "mock")
    if name == "mock":
        return MockProvider()
    if name == "hosted":
        return HostedProvider(os.environ.get("SWITCHYARD_AGENT_MODEL_URL"))
    raise SystemExit("unknown provider: " + name)


def fetch_context(base, token, project, environment):
    request = urllib.request.Request(
        f"{base}/v1/projects/{project}/agent/context?environment_id={environment}",
        headers={"Authorization": "Bearer " + token},
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        return EnvironmentContext.model_validate_json(response.read())


def submit(base, token, project, draft):
    request = urllib.request.Request(
        f"{base}/v1/projects/{project}/agent/proposals",
        data=json.dumps(draft.request_body()).encode(),
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default=os.environ.get("SWITCHYARD_URL", "http://127.0.0.1:8080"))
    parser.add_argument("--token", default=os.environ.get("SWITCHYARD_AGENT_TOKEN", ""))
    parser.add_argument("--project", required=True)
    parser.add_argument("--environment", required=True)
    parser.add_argument("--dry-run", action="store_true", help="Print the suggestion and do not call the API")
    parser.add_argument("--context-file", help="Read a saved context instead of the API")
    args = parser.parse_args(argv)
    if args.context_file:
        with open(args.context_file, encoding="utf-8") as handle:
            summary = EnvironmentContext.model_validate_json(handle.read())
    else:
        if not args.token:
            raise SystemExit("set --token or SWITCHYARD_AGENT_TOKEN")
        summary = fetch_context(args.base_url, args.token, args.project, args.environment)
    draft = provider_from_env().suggest(summary)
    if args.dry_run:
        print(draft.model_dump_json(indent=2))
        return 0
    if not args.token:
        raise SystemExit("set --token or SWITCHYARD_AGENT_TOKEN")
    print(json.dumps(submit(args.base_url, args.token, args.project, draft), indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
