// Round two of the differential: abstract types, the list-nullability matrix,
// nested input coercion and directives on fragments.
import { readFileSync } from 'node:fs';
import {
  graphql, GraphQLSchema, GraphQLObjectType, GraphQLInterfaceType, GraphQLUnionType,
  GraphQLString, GraphQLInt, GraphQLBoolean,
  GraphQLList, GraphQLNonNull, GraphQLEnumType, GraphQLInputObjectType,
  GraphQLError,
} from 'graphql';


// Both engines describe what they received with the same rules, rather than
// JSON.stringify against a Go struct: absent and null are different answers and
// a struct cannot omit a field. "absent" means the key never arrived.
const has = (o, k) => o != null && Object.prototype.hasOwnProperty.call(o, k);
const scalar = (o, k) => (!has(o, k) ? 'absent' : o[k] === null ? 'null' : String(o[k]));
const fmtInner = (v) => (v === undefined ? 'absent' : v === null ? 'null'
  : `Inner{n=${scalar(v, 'n')},s=${scalar(v, 's')}}`);
const fmtInnerList = (v) => (v === undefined ? 'absent' : v === null ? 'null'
  : `[${v.map(fmtInner).join(',')}]`);
const fmtOuter = (v) => (v === undefined ? 'absent' : v === null ? 'null'
  : `Outer{inner=${fmtInner(has(v, 'inner') ? v.inner : undefined)}`
    + `,list=${fmtInnerList(has(v, 'list') ? v.list : undefined)}`
    + `,size=${scalar(v, 'size')},req=${scalar(v, 'req')}}`);

const Size = new GraphQLEnumType({ name: 'Size', values: { SMALL: {}, LARGE: {} } });

const Inner = new GraphQLInputObjectType({
  name: 'Inner',
  fields: {
    n: { type: GraphQLInt },
    s: { type: GraphQLString, defaultValue: 'inner-default' },
  },
});
const Outer = new GraphQLInputObjectType({
  name: 'Outer',
  fields: {
    inner: { type: Inner },
    list: { type: new GraphQLList(Inner) },
    size: { type: Size, defaultValue: 'SMALL' },
    req: { type: new GraphQLNonNull(GraphQLInt), defaultValue: 7 },
  },
});

// Two real implementations, resolved by a field on the value -- the ambiguity
// in round one was the Go fixture binding both names to one Go type.
const Named = new GraphQLInterfaceType({
  name: 'Named',
  fields: { name: { type: new GraphQLNonNull(GraphQLString) } },
  resolveType: (v) => v.kind,
});
const Dog = new GraphQLObjectType({
  name: 'Dog', interfaces: [Named],
  fields: {
    name: { type: new GraphQLNonNull(GraphQLString) },
    barks: { type: new GraphQLNonNull(GraphQLBoolean) },
  },
});
const Cat = new GraphQLObjectType({
  name: 'Cat', interfaces: [Named],
  fields: {
    name: { type: new GraphQLNonNull(GraphQLString) },
    lives: { type: new GraphQLNonNull(GraphQLInt) },
  },
});
const Pet = new GraphQLUnionType({ name: 'Pet', types: [Dog, Cat], resolveType: (v) => v.kind });

const lists = {
  // [String]
  nn: { type: new GraphQLList(GraphQLString), resolve: () => ['a', null, 'c'] },
  // [String!]
  nx: { type: new GraphQLList(new GraphQLNonNull(GraphQLString)), resolve: () => ['a', null, 'c'] },
  // [String]!
  xn: { type: new GraphQLNonNull(new GraphQLList(GraphQLString)), resolve: () => ['a', null, 'c'] },
  // [String!]!
  xx: { type: new GraphQLNonNull(new GraphQLList(new GraphQLNonNull(GraphQLString))), resolve: () => ['a', null, 'c'] },
  // the whole list null, in each shape
  nnNull: { type: new GraphQLList(GraphQLString), resolve: () => null },
  xnNull: { type: new GraphQLNonNull(new GraphQLList(GraphQLString)), resolve: () => null },
  // nested
  deepNN: { type: new GraphQLList(new GraphQLList(GraphQLString)), resolve: () => [['a'], [null], null] },
  deepXX: {
    type: new GraphQLNonNull(new GraphQLList(new GraphQLNonNull(new GraphQLList(new GraphQLNonNull(GraphQLString))))),
    resolve: () => [['a'], [null], ['c']],
  },
};

const Query = new GraphQLObjectType({
  name: 'Query',
  fields: {
    ...lists,
    named: { type: Named, resolve: () => ({ kind: 'Dog', name: 'Rex', barks: true }) },
    namedCat: { type: Named, resolve: () => ({ kind: 'Cat', name: 'Tom', lives: 9 }) },
    pet: { type: Pet, resolve: () => ({ kind: 'Dog', name: 'Rex', barks: true }) },
    namedList: {
      type: new GraphQLList(Named),
      resolve: () => [
        { kind: 'Dog', name: 'Rex', barks: true },
        { kind: 'Cat', name: 'Tom', lives: 9 },
      ],
    },
    outerArg: {
      type: GraphQLString,
      args: { v: { type: Outer } },
      resolve: (_, a) => fmtOuter(a.v),
    },
    innerListArg: {
      type: GraphQLString,
      args: { v: { type: new GraphQLList(Inner) } },
      resolve: (_, a) => fmtInnerList(a.v),
    },
    sizeArg: {
      type: GraphQLString,
      args: { v: { type: Size, defaultValue: 'LARGE' } },
      resolve: (_, a) => String(a.v),
    },
    str: { type: new GraphQLNonNull(GraphQLString), resolve: () => 'ok' },
    boom: { type: GraphQLString, resolve: () => { throw new GraphQLError('boom'); } },
  },
});

const schema = new GraphQLSchema({ query: Query, types: [Dog, Cat] });

const cases = JSON.parse(readFileSync(new URL('./cases2.json', import.meta.url), 'utf8'));
const out = [];
for (const c of cases) {
  const r = await graphql({ schema, source: c.query, variableValues: c.variables });
  out.push({
    name: c.name,
    hasData: Object.prototype.hasOwnProperty.call(r, 'data'),
    data: r.data === undefined ? null : r.data,
    errors: (r.errors || []).map((e) => ({ path: e.path || null, message: e.message })),
    errorCount: (r.errors || []).length,
  });
}
console.log(JSON.stringify(out, null, 1));
