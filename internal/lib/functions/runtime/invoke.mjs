// Function runtime for Node.js, run once per invocation inside the function's warm
// container. It reads {"event", "context"} as JSON on stdin, loads the handler named by
// _HANDLER ("file.export", as on AWS Lambda) from /var/task, and writes exactly one JSON
// line to stdout: {"ok":true,"result":...} or {"ok":false,"errorType",...}.
// Everything the handler prints goes to stderr, which becomes the invocation log.
import { existsSync } from "node:fs";
import { pathToFileURL } from "node:url";

const out = process.stdout.write.bind(process.stdout);
for (const k of ["log", "info", "debug", "warn", "trace"]) console[k] = (...a) => console.error(...a);
process.stdout.write = (chunk, ...rest) => process.stderr.write(chunk, ...rest);

function emit(obj) {
  out(JSON.stringify(obj) + "\n", () => process.exit(0));
}
function fail(type, message, stack) {
  emit({ ok: false, errorType: type, errorMessage: message, stack: stack ? String(stack).split("\n") : [] });
}
process.on("uncaughtException", (e) => fail(e?.name || "Error", e?.message || String(e), e?.stack));
process.on("unhandledRejection", (e) => fail(e?.name || "Error", e?.message || String(e), e?.stack));

let input = "";
for await (const chunk of process.stdin) input += chunk;
const { event, context } = JSON.parse(input || "{}");

const spec = process.env._HANDLER || "index.handler";
const dot = spec.lastIndexOf(".");
const file = spec.slice(0, dot), name = spec.slice(dot + 1);
let mod;
for (const ext of [".mjs", ".js", ".cjs"]) {
  const path = "/var/task/" + file + ext;
  if (existsSync(path)) {
    try {
      mod = await import(pathToFileURL(path).href);
    } catch (e) {
      fail("Runtime.UserCodeSyntaxError", e?.message || String(e), e?.stack);
      await new Promise(() => {});
    }
    break;
  }
}
if (!mod) {
  fail("Runtime.ImportModuleError", `Cannot find module '${file}' (looked for ${file}.mjs, ${file}.js, ${file}.cjs)`);
} else {
  const handler = mod[name] ?? mod.default?.[name];
  if (typeof handler !== "function") {
    fail("Runtime.HandlerNotFound", `${file}.${name} is undefined or not exported`);
  } else {
    const ctx = {
      ...context,
      callbackWaitsForEmptyEventLoop: false,
      getRemainingTimeInMillis: () => Math.max(0, (context?.deadlineMs ?? Date.now()) - Date.now()),
    };
    try {
      const result = await handler(event, ctx);
      emit({ ok: true, result: result === undefined ? null : result });
    } catch (e) {
      fail(e?.name || "Error", e?.message || String(e), e?.stack);
    }
  }
}
