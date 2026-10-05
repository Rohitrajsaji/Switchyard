# ADR 0006: Immutable snapshots with bounded authoritative age

Status: accepted for M6; snapshot/Redis storage verified, coordinator/API integration in progress.

## Decision

Cache a configuration per project/environment/flag key. Do not cache user assignments or put attributes/user identities into cache keys. `pkg/snapshot` is the shared wire/compilation boundary so future SDKs use the same version and expiry rules as the API.

Version 1 wraps an immutable definition and `verified_at`. Capture that timestamp **before** the authoritative PostgreSQL query; pool wait and query execution consume freshness instead of extending it. Only a successful authoritative read produces a new verification timestamp. Redis reads, intermediary responses and process restarts preserve it. Reject unknown versions/fields, trailing data, mismatched scope, invalid definitions and payloads above 1 MiB. Evaluate only while age is strictly below 30 seconds; reverify after two seconds. A timestamp ahead of the reader's clock is conservatively unusable until its clock reaches that timestamp. These rules assume bounded host clock skew; they do not create a distributed clock service.

Compile once, own every byte slice, and share immutable pointers between readers. Caller edits and returned JSON/value bytes cannot mutate the stored configuration. The forthcoming coordinator will bound both cache admission and concurrent refresh, replace whole pointers, coalesce misses and repair from PostgreSQL. PostgreSQL remains the authorization and configuration truth; Redis availability will not be a prerequisite for API startup or readiness.

Redis stores each envelope in one hash. One Lua script compares canonical integer-string revisions and atomically updates definition, verification metadata, payload and TTL. Comparing revisions as Lua floating-point numbers would lose increments above 2^53, so compare decimal length then lexical order. Older revisions cannot overwrite newer disables. Equal revisions may update their proof timestamp only with identical configuration and flag identity. Reusing an unchanged proof does not renew TTL. TTL is the remaining authoritative age, rounded down to milliseconds; readers independently enforce expiry even if network delay lets a Redis key live slightly longer. Redis eviction/deletion loses cache history, so a monotonic fence applies to the currently present entry, not across a total cache reset. Authoritative polling and age checks provide repair and bounded staleness after resets.

Use the pinned official [`go-redis` client](https://redis.io/docs/latest/develop/clients/go/) 9.22.0 with a four-connection cap, explicit subsecond pool/connect/read/write deadlines, context deadlines and automatic retries disabled. Experimental client-side caching/automatic pipelines are unnecessary for this design. Redis 8.10.2 is pinned to a verified multi-architecture image digest from the [official image catalog](https://github.com/docker-library/official-images/blob/master/library/redis). The local server runs as the Redis user, has a 128 MiB container limit, a 96 MiB eviction ceiling, no persistence and localhost-only port access. PostgreSQL owns durable state. During M6 construction Redis is opt-in through the `cache` Compose profile.

## Evidence and limits

Injected-clock snapshot tests verify exact two-/thirty-second boundaries, cache relay/restart without renewal, future-time rejection, immutable byte ownership, concurrent evaluation and kill-switch precedence. Real Redis integration checks delayed concurrent writes, >2^53 and maximum-int64 revisions, identity/scope conflicts, corruption rejection, deletion repair and remaining-age TTL using only test-owned keys.

The API still evaluates directly from PostgreSQL at this checkpoint. Bounded refresh, cache counters, propagation/outage drills and the complete M6 gate are pending. No cache throughput, high-availability or propagation performance is claimed yet.
