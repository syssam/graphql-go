// Runs the official GraphQL-over-HTTP audit suite against a URL.
// Usage: node run.mjs http://127.0.0.1:4150/
import { auditServer } from 'graphql-http';

const url = process.argv[2];
const results = await auditServer({ url, fetchFn: fetch });

const ok = [];
const warn = [];
const fail = [];
for (const r of results) {
  if (r.status === 'ok') ok.push(r);
  else if (r.status === 'warn') warn.push(r);
  else fail.push(r);
}

console.log(`total ${results.length}: ok ${ok.length}, warn ${warn.length}, error ${fail.length}\n`);

const show = (label, list) => {
  if (!list.length) return;
  console.log(`--- ${label} (${list.length}) ---`);
  for (const r of list) {
    console.log(`  [${r.id}] ${r.name}`);
    if (r.reason) console.log(`      ${String(r.reason).split('\n')[0].slice(0, 160)}`);
  }
  console.log();
};
show('ERROR (MUST)', fail);
show('WARN (SHOULD/MAY)', warn);
console.log(`--- OK (${ok.length}) ---`);
for (const r of ok) console.log(`  ${r.name}`);
