# Switchyard JavaScript client

Remote evaluation only. The Go server decides the value. `bucket` repeats the published assignment hash so this client can check `testdata/evaluation/golden.json` without becoming a second evaluator.

```js
import { evaluate } from "./evaluate.mjs";

const decision = await evaluate({
  baseUrl: "http://127.0.0.1:8080",
  token: process.env.SWITCHYARD_APP_TOKEN,
  projectId,
  environmentId,
  key: "copy",
  userId,
  fallback: { type: "string", data: "safe" },
});
```

Run `node --test sdk/javascript/evaluate.test.mjs` from the repository root.
