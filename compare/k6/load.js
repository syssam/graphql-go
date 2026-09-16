// k6 load test, run against one engine's server at a time.
//
//   go run gen.go -n 200
//   go build -o srv.exe ./graphqlgo/cmd/graphqlgo-server && ./srv.exe -addr :18080
//   k6 run -e URL=http://localhost:18080/graphql -e ENGINE=graphql-go k6/load.js
//
// The queries are the same six the in-process comparison uses, so a figure
// here is comparable with the ones in ../README.md rather than measuring a
// different workload.
//
// Read throughput, not percentiles. This machine's monotonic clock has about
// 522us of granularity and a request here takes well under that, so every
// per-request duration k6 reports is quantised to a multiple of the tick. The
// faster engine accumulates more sub-tick samples, which biases percentiles
// toward the slower one. Throughput spans millions of ticks and is unaffected.
// No load tool escapes this: they all ask the same OS clock.
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const URL = __ENV.URL || 'http://localhost:18080/graphql';
const ENGINE = __ENV.ENGINE || 'unknown';

// Counted rather than inferred from failures: a GraphQL server answers 200
// with an errors member, so a status check alone would call a broken engine
// healthy.
const graphqlErrors = new Counter('graphql_errors');

const QUERIES = {
  leaf: '{ entity000(id: "3") { id name description active score ratio tags } }',
  owner: '{ entity000(id: "3") { id owner { id name } } }',
  connection: `{ entity000s(first: 5) {
    totalCount
    pageInfo { hasNextPage startCursor endCursor }
    edges { cursor node { id name } }
  } }`,
  nested: `{ entity000s(first: 3) {
    edges { node { id children(first: 2) { totalCount edges { node { id name } } } } }
  } }`,
  ownerChain: `{ entity000s(first: 2) {
    edges { node { id owner { id owner { id name } } } }
  } }`,
  mutation:
    'mutation { updateEntity000(id: "1", input: { name: "renamed" }) { id name } }',
};

// MODE=fixed offers a rate both engines can serve and compares latency at
// equal load. MODE=ramp probes for a ceiling, but on one machine that ceiling
// is the machine's: k6 and the server share the CPU, and raising maxVUs from
// 400 to 2000 cut measured throughput from 9.0k to 4.9k req/s because the
// generator took cores from the thing it was measuring. Ramp numbers are the
// pair, not the server. A capacity figure needs a second host.
const MODE = __ENV.MODE || 'fixed';
const RATE = Number(__ENV.RATE || 3000);

const scenarios = {
  fixed: {
    executor: 'constant-arrival-rate',
    rate: RATE,
    timeUnit: '1s',
    duration: __ENV.DURATION || '30s',
    preAllocatedVUs: Number(__ENV.PREVUS || 100),
    maxVUs: Number(__ENV.MAXVUS || 400),
  },
  ramp: {
    executor: 'ramping-arrival-rate',
    startRate: 200,
    timeUnit: '1s',
    preAllocatedVUs: Number(__ENV.PREVUS || 50),
    maxVUs: Number(__ENV.MAXVUS || 400),
    stages: [
      { target: 2000, duration: '20s' },
      { target: 8000, duration: '20s' },
      { target: 20000, duration: '30s' },
      { target: 20000, duration: '20s' },
    ],
  },
};

export const options = {
  scenarios: { [MODE]: scenarios[MODE] },
  thresholds: {
    checks: ['rate>0.99'],
    graphql_errors: ['count==0'],
  },
  discardResponseBodies: false,
  tags: { engine: ENGINE },
};

const names = Object.keys(QUERIES);

export default function () {
  const name = names[__ITER % names.length];
  const res = http.post(URL, JSON.stringify({ query: QUERIES[name] }), {
    headers: { 'Content-Type': 'application/json' },
    tags: { query: name },
  });

  const ok = check(res, {
    'status 200': (r) => r.status === 200,
    'has data': (r) => r.body && r.body.indexOf('"data"') !== -1,
  });
  if (!ok || (res.body && res.body.indexOf('"errors"') !== -1)) {
    graphqlErrors.add(1, { query: name });
  }
}
