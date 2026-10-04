"""Real HTTP management journey using explicitly seeded local-only credentials."""
import http.cookiejar
import json
import os
import urllib.error
import urllib.request
import uuid

password = os.environ["SWITCHYARD_DEMO_PASSWORD"]
base = "http://localhost:8080"
origin = os.environ.get("SWITCHYARD_ORIGIN", "http://localhost:3000")


class Client:
    def __init__(self, email):
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar())
        )
        self.csrf = ""
        status, data = self.call("POST", "/v1/session", {"email": email, "password": password})
        assert status == 200, f"Login status {status}"
        self.csrf = data["csrf_token"]

    def call(self, method, path, data=None, csrf=True):
        headers = {"Origin": origin, "Content-Type": "application/json"}
        if csrf:
            headers["X-CSRF-Token"] = self.csrf
        request = urllib.request.Request(
            base + path,
            data=None if data is None else json.dumps(data).encode(),
            headers=headers,
            method=method,
        )
        try:
            response = self.opener.open(request, timeout=5)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            body = response.read()
            return response.code, json.loads(body) if body else None


def main():
    admin = Client("admin@example.test")
    viewer = Client("viewer@example.test")
    developer = Client("developer@example.test")
    status, project = admin.call("POST", "/v1/projects", {"name": "Smoke " + uuid.uuid4().hex[:8]})
    assert status == 201
    path = "/v1/projects/" + project["id"]
    assert viewer.call("GET", path + "/environments")[0] == 403
    for user in ["demo_viewer", "demo_developer"]:
        assert admin.call("POST", path + "/members", {"user_id": user})[0] == 204
    assert viewer.call("GET", path + "/environments")[0] == 200
    assert viewer.call("POST", "/v1/projects", {"name": "Forbidden"})[0] == 403
    status, environments = admin.call("GET", path + "/environments")
    assert status == 200
    env = {item["name"]: item["id"] for item in environments}
    assert set(env) == {"development", "staging", "production"}
    assert admin.call("POST", path + "/application-keys", {
        "name": "Forbidden production", "environment_id": env["production"], "permissions": ["evaluate"]
    })[0] == 403
    status, key = developer.call("POST", path + "/application-keys", {
        "name": "Smoke Go app", "environment_id": env["development"], "permissions": ["evaluate", "config:read"]
    })
    assert status == 201 and key["token"].startswith("swk_")
    assert developer.call("DELETE", path + "/application-keys/" + key["id"])[0] == 204
    assert admin.call("POST", "/v1/projects", {"name": "No CSRF"}, csrf=False)[0] == 403
    status, entries = viewer.call("GET", path + "/audit")
    assert status == 200 and len(entries) == 5
    assert key["token"] not in json.dumps(entries)
    for client in [admin, viewer, developer]:
        assert client.call("DELETE", "/v1/session")[0] == 204
        assert client.call("GET", "/v1/session")[0] == 401
    print("Live management passed: three roles, memberships, production denial, key revoke, CSRF, audit, logout")


if __name__ == "__main__":
    main()
