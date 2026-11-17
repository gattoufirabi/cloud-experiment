# Function runtime for Python, run once per invocation inside the function's warm
# container. It reads {"event", "context"} as JSON on stdin, imports the handler named by
# _HANDLER ("module.function", as on AWS Lambda) from /var/task, and writes exactly one
# JSON line to stdout: {"ok": true, "result": ...} or {"ok": false, "errorType": ...}.
# Everything the handler prints goes to stderr, which becomes the invocation log.
import importlib
import json
import os
import sys
import time
import traceback

real_stdout = sys.stdout
sys.stdout = sys.stderr


def emit(obj):
    real_stdout.write(json.dumps(obj) + "\n")
    real_stdout.flush()
    sys.exit(0)


def fail(kind, message, exc=True):
    emit({"ok": False, "errorType": kind, "errorMessage": message,
          "stack": traceback.format_exc().splitlines() if exc else []})


class Context:
    def __init__(self, d):
        self.function_name = d.get("functionName", "")
        self.function_version = d.get("functionVersion", "$LATEST")
        self.invoked_function_arn = d.get("invokedFunctionArn", "")
        self.memory_limit_in_mb = d.get("memoryLimitInMB", 128)
        self.aws_request_id = d.get("awsRequestId", "")
        self.log_group_name = d.get("logGroupName", "")
        self.log_stream_name = d.get("logStreamName", "")
        self._deadline_ms = d.get("deadlineMs", int(time.time() * 1000))

    def get_remaining_time_in_millis(self):
        return max(0, self._deadline_ms - int(time.time() * 1000))


data = json.loads(sys.stdin.buffer.read() or b"{}")
spec = os.environ.get("_HANDLER", "lambda_function.lambda_handler")
module_name, _, func_name = spec.rpartition(".")
sys.path.insert(0, "/var/task")
try:
    module = importlib.import_module(module_name.replace("/", "."))
except SyntaxError as e:
    fail("Runtime.UserCodeSyntaxError", str(e))
except Exception as e:  # noqa: BLE001 - any import failure is reported to the caller
    fail("Runtime.ImportModuleError", "Unable to import module '%s': %s" % (module_name, e))

handler = getattr(module, func_name, None)
if not callable(handler):
    fail("Runtime.HandlerNotFound", "Handler '%s' missing on module '%s'" % (func_name, module_name), exc=False)

try:
    result = handler(data.get("event"), Context(data.get("context") or {}))
except Exception as e:  # noqa: BLE001
    fail(type(e).__name__, str(e))

try:
    real_stdout.write(json.dumps({"ok": True, "result": result}) + "\n")
    real_stdout.flush()
except (TypeError, ValueError) as e:
    fail("Runtime.MarshalError", "Unable to marshal response: %s" % e, exc=False)
