"""Replaceable hosted adapter. It does not call a network unless a URL is supplied, and nothing supplies one by default."""


class HostedProvider:
    def __init__(self, url: str | None):
        self.url = (url or "").strip()

    def suggest(self, summary):
        if not self.url:
            raise RuntimeError("no hosted model is configured")
        raise RuntimeError("hosted output is not invoked by the default agent")
