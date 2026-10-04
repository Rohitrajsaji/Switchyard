"""Read-only HTTP smoke check; exits nonzero on any failed assertion."""
import json
import os
import urllib.request

base = os.environ.get("SWITCHYARD_URL", "http://127.0.0.1:8080")
for path, expected in [("/health/live", "alive"), ("/health/ready", "ready")]:
    with urllib.request.urlopen(base + path, timeout=5) as response:
        assert response.status == 200
        assert response.headers["X-Request-ID"]
        assert json.load(response)["status"] == expected
print("HTTP liveness/readiness smoke passed")
