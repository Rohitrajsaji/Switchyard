# ADR 0002: human sessions, scoped application keys and transactional audit

Status: accepted implementation of the approved M2 design.

Human accounts use organization roles plus explicit project membership. An admin is not implicitly a member of every project. Developers/admins can create projects; only an admin member grants memberships. Services re-read current roles and memberships inside write transactions instead of trusting transport-supplied role fields. Production configuration is read-only until M9 adds the central approval policy.

Passwords use bcrypt cost 12 and 12–72 UTF-8 bytes. Opaque 256-bit session tokens are stored only as SHA-256 hashes, expire in 24 hours and are revoked on logout. Cookies are HttpOnly and SameSite=Strict; Secure is configurable and disabled only for local HTTP. Mutation requests require an exact configured Origin and a session-bound CSRF token. The CSRF token derives from the secret session token with a domain-separated SHA-256 input, can be reissued on page reload, and is also stored only as a hash. Browser JavaScript never receives the session token in JSON.

Application keys are separate opaque tokens with hashed storage and project/environment/permission scope. Their permissions are evaluate, events:write and config:read; none authorize human management. Tokens are returned once and never put in audit details or request logs. Revocation is immediate on subsequent authenticated requests.

Audit entries share the domain write's PostgreSQL transaction. A failed audit insert fails the write, and a failed write leaves no success entry. Update/delete/truncate triggers protect history from accidental application SQL; the database owner can still alter the schema. This is append-only application history, not cryptographic tamper proofing.

Login is bounded to ten attempts/minute/direct peer IP and management to 600 requests/minute. The limiter has a maximum 4,096 entries and is safe under concurrency. Forwarded IP headers are not trusted. The eventual local Next.js proxy shares one peer address, so this small demo limiter protects resources rather than claiming distributed/global abuse prevention. Replace it only if hosted requirements justify an authenticated shared limiter.

Demo users require explicit seed opt-in and an explicitly supplied demo password; normal startup creates no accounts. Two separate demo admins support the later proposer/reviewer separation.
