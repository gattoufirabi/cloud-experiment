// k6 profiles for scripts/loadtest/loadtest.sh. Each step is its own scenario, so the
// summary has one set of numbers per step.
//
//   PROFILE=break  URL, HOST, STEPS (req/s, comma separated), HOLD (s per step):
//                  constant arrival rate per step; the run stops at the first step where
//                  more than ABORT_ERR of requests fail or p95 passes ABORT_P95 ms.
//   PROFILE=conns  URL, HOST, STEPS (concurrent connections), HOLD: each virtual user keeps
//                  one request in flight (use /sleep so each one holds its connection).
//   PROFILE=spread URL path, HOSTS (comma separated), RATE, HOLD: one step spread over
//                  many applications (density check).
import http from 'k6/http';

const PROFILE = __ENV.PROFILE || 'break';
const HOLD = Number(__ENV.HOLD || 30);
const steps = (__ENV.STEPS || '').split(',').filter(Boolean).map(Number);
const scenarios = {};
const pad = (n) => String(n).padStart(6, '0');
// at most this many requests in flight: once the platform slows down, k6 counts what it
// could not send ("not sent") instead of growing without bound
const MAXVUS = Number(__ENV.MAXVUS || 4000);

if (PROFILE === 'break') {
  steps.forEach((r, i) => {
    scenarios['s' + pad(r)] = { executor: 'constant-arrival-rate', exec: 'hit', rate: r, timeUnit: '1s',
      duration: `${HOLD}s`, startTime: `${i * HOLD}s`,
      preAllocatedVUs: Math.min(MAXVUS, Math.max(50, Math.ceil(r / 4))), maxVUs: Math.min(MAXVUS, Math.max(500, r * 2)) };
  });
} else if (PROFILE === 'conns') {
  steps.forEach((v, i) => {
    scenarios['s' + pad(v)] = { executor: 'constant-vus', exec: 'hit', vus: v, duration: `${HOLD}s`,
      startTime: `${i * HOLD}s`, gracefulStop: '5s' };
  });
} else if (PROFILE === 'spread') {
  const r = Number(__ENV.RATE || 200);
  scenarios['s' + pad(r)] = { executor: 'constant-arrival-rate', exec: 'spread', rate: r, timeUnit: '1s',
    duration: `${HOLD}s`, preAllocatedVUs: Math.max(50, r), maxVUs: Math.max(500, r * 4) };
}

const thresholds = {};
for (const n of Object.keys(scenarios)) {
  const abort = PROFILE !== 'spread';
  thresholds[`http_req_duration{scenario:${n}}`] = abort
    ? ['max>=0', { threshold: `p(95)<${__ENV.ABORT_P95 || 5000}`, abortOnFail: true, delayAbortEval: '10s' }] : ['max>=0'];
  thresholds[`http_req_failed{scenario:${n}}`] = abort
    ? [{ threshold: `rate<${__ENV.ABORT_ERR || 0.1}`, abortOnFail: true, delayAbortEval: '10s' }] : ['rate>=0'];
  thresholds[`http_reqs{scenario:${n}}`] = ['count>=0'];
  thresholds[`dropped_iterations{scenario:${n}}`] = ['count>=0'];
  thresholds[`data_received{scenario:${n}}`] = ['count>=0'];
}

export const options = {
  scenarios, thresholds,
  discardResponseBodies: true,
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  insecureSkipTLSVerify: false,
};

const params = { headers: __ENV.HOST ? { Host: __ENV.HOST } : {}, timeout: __ENV.TIMEOUT || '10s' };
export function hit() { http.get(__ENV.URL, params); }

const hosts = (__ENV.HOSTS || '').split(',').filter(Boolean);
export function spread() {
  const h = hosts[Math.floor(Math.random() * hosts.length)];
  http.get(`https://${h}${__ENV.URL || '/'}`, { timeout: __ENV.TIMEOUT || '10s' });
}

export function handleSummary(data) {
  return { [`/out/${__ENV.NAME}.summary.json`]: JSON.stringify(data) };
}
