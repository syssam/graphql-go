import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

// A closed model with a fixed number of VUs, not a fixed arrival rate: an
// open model on a shared machine reports the generator's dropped iterations as
// if they were the server's, which is what k6/README.md found. With N VUs each
// waiting for its response, throughput is whatever the server can do and the
// generator cannot run ahead of it.
const VUS = Number(__ENV.VUS || 16);
const DUR = __ENV.DURATION || '20s';

export const options = {
  scenarios: {
    load: { executor: 'constant-vus', vus: VUS, duration: DUR, gracefulStop: '5s' },
  },
  // No thresholds: this script reports, it does not judge.
  summaryTrendStats: ['min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

const QUERIES = {
  tiny: '{ users { id } }',
  shallow: '{ users { id name email } }',
  nested: '{ users { id friends { id name } } }',
};

const url = __ENV.URL;
const query = QUERIES[__ENV.SIZE || 'shallow'];
const bodyBytes = new Trend('response_bytes');

export default function () {
  const res = http.post(url, JSON.stringify({ query }), {
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
  });
  bodyBytes.add(res.body ? res.body.length : 0);
  check(res, {
    'status 200': (r) => r.status === 200,
    // A server that answers 200 with an errors array is not doing the work.
    'no errors': (r) => r.body && r.body.indexOf('"errors"') === -1,
  });
}

// One line of JSON on stdout, so a runner can collect rounds without parsing
// k6's human summary.
export function handleSummary(data) {
  const d = data.metrics.http_req_duration.values;
  const r = data.metrics.http_reqs.values;
  const failed = data.metrics.checks ? data.metrics.checks.values.fails : -1;
  return {
    stdout: JSON.stringify({
      rps: r.rate, count: r.count, failed,
      med: d.med, p90: d['p(90)'], p95: d['p(95)'], p99: d['p(99)'], max: d.max,
    }) + '\n',
  };
}
