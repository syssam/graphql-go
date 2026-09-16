// k6 against one transport at a time, so the Go benchmarks in
// transport_bench_test.go are not the only evidence for what a transport
// costs.
//
//   go run ./cmd/transportserver -transport fiber -addr :18090
//   k6 run -e URL=http://localhost:18090/graphql -e ENGINE=fiber k6/transports.js
//
// Two payload sizes, because per-request transport overhead is a fixed cost: a
// list response hides it and a tiny one isolates it, and only the pair says
// whether it matters in practice. Set SIZE=tiny or SIZE=list.
//
// Run the transports interleaved, not one after the other. This machine runs
// other work, and a run that happens to coincide with a compile will be slow
// for whichever transport was unlucky.
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const URL = __ENV.URL || 'http://localhost:18090/graphql';
const ENGINE = __ENV.ENGINE || 'unknown';
const SIZE = __ENV.SIZE || 'tiny';

const graphqlErrors = new Counter('graphql_errors');

// tiny returns one field of one user; list returns every user with friends.
// The first is almost entirely transport, the second almost entirely engine.
const QUERIES = {
  tiny: '{ users { id } }',
  list: '{ users { id name email friends { id name } } }',
};

export const options = {
  scenarios: {
    fixed: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 3000),
      timeUnit: '1s',
      duration: __ENV.DURATION || '12s',
      preAllocatedVUs: Number(__ENV.PREVUS || 100),
      maxVUs: Number(__ENV.MAXVUS || 400),
    },
  },
  thresholds: { checks: ['rate>0.99'], graphql_errors: ['count==0'] },
  tags: { engine: ENGINE, size: SIZE },
};

const BODY = JSON.stringify({ query: QUERIES[SIZE] });

export default function () {
  const res = http.post(URL, BODY, {
    headers: { 'Content-Type': 'application/json' },
  });
  const ok = check(res, {
    'status 200': (r) => r.status === 200,
    'has data': (r) => r.body && r.body.indexOf('"data"') !== -1,
  });
  if (!ok || (res.body && res.body.indexOf('"errors"') !== -1)) {
    graphqlErrors.add(1);
  }
}
