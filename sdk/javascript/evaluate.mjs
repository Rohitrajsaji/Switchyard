// Remote evaluation client. Assignment buckets match pkg/evaluation.Bucket:
// SHA-256 over eight length-prefixed UTF-8 fields, first 8 bytes as uint64 big-endian, modulo 10000.

import { createHash } from "node:crypto";

export function bucket(projectID, environmentID, flagID, runID, purpose, salt, userID) {
  const hash = createHash("sha256");
  for (const field of ["v1", projectID, environmentID, flagID, runID, purpose, salt, userID]) {
    const bytes = Buffer.from(field, "utf8");
    const length = Buffer.alloc(4);
    length.writeUInt32BE(bytes.length);
    hash.update(length);
    hash.update(bytes);
  }
  return Number(hash.digest().readBigUInt64BE(0) % 10000n);
}

export async function evaluate({ baseUrl, token, projectId, environmentId, key, userId, attributes, fallback, fetchImpl = fetch }) {
  const response = await fetchImpl(new URL("/v1/evaluate", baseUrl), {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({
      project_id: projectId,
      environment_id: environmentId,
      key,
      user_id: userId,
      attributes: attributes ?? {},
      fallback,
    }),
  });
  const body = await response.json();
  if (!response.ok) {
    const error = new Error(body.error ?? "evaluation_failed");
    error.status = response.status;
    error.body = body;
    throw error;
  }
  return body;
}
