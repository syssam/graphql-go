import { readFileSync } from 'node:fs';
import {
  graphql, GraphQLSchema, GraphQLObjectType, GraphQLInterfaceType,
  GraphQLString, GraphQLInt, GraphQLFloat, GraphQLID, GraphQLBoolean,
  GraphQLList, GraphQLNonNull, GraphQLEnumType, GraphQLInputObjectType,
  GraphQLError,
} from 'graphql';

const Color = new GraphQLEnumType({ name: 'Color', values: { RED: {}, GREEN: {} } });
const Inp = new GraphQLInputObjectType({
  name: 'Inp',
  fields: { n: { type: new GraphQLNonNull(GraphQLInt) } },
});

const Node = new GraphQLInterfaceType({
  name: 'Node',
  fields: { id: { type: new GraphQLNonNull(GraphQLID) } },
  resolveType: () => 'Thing',
});
const Thing = new GraphQLObjectType({
  name: 'Thing', interfaces: [Node],
  fields: { id: { type: new GraphQLNonNull(GraphQLID) } },
});
const Other = new GraphQLObjectType({
  name: 'Other', interfaces: [Node],
  fields: { id: { type: new GraphQLNonNull(GraphQLID) } },
});

const Obj = new GraphQLObjectType({
  name: 'Obj',
  fields: {
    failNonNull: {
      type: new GraphQLNonNull(GraphQLString),
      resolve: () => { throw new GraphQLError('boom'); },
    },
    ok: { type: GraphQLString, resolve: () => 'x' },
  },
});

const Query = new GraphQLObjectType({
  name: 'Query',
  fields: {
    str: { type: new GraphQLNonNull(GraphQLString), resolve: () => 'ok' },
    failNullable: { type: GraphQLString, resolve: () => { throw new GraphQLError('boom'); } },
    failNonNull: {
      type: new GraphQLNonNull(GraphQLString),
      resolve: () => { throw new GraphQLError('boom'); },
    },
    nullFromNonNull: { type: new GraphQLNonNull(GraphQLString), resolve: () => null },
    nullableObj: { type: Obj, resolve: () => ({}) },
    node: { type: Node, resolve: () => ({ id: '1' }) },
    listNullableElems: {
      type: new GraphQLList(GraphQLString),
      resolve: () => ['a', null, 'c'].map((v, i) => {
        if (i === 1) throw new GraphQLError('boom');
        return v;
      }),
    },
    listNonNullElems: {
      type: new GraphQLList(new GraphQLNonNull(GraphQLString)),
      resolve: () => ['a', null, 'c'],
    },
    grid: {
      type: new GraphQLList(new GraphQLList(new GraphQLNonNull(GraphQLString))),
      resolve: () => [['a'], [null], ['c']],
    },
    withDefault: {
      type: GraphQLString,
      args: { v: { type: GraphQLString, defaultValue: 'D' } },
      resolve: (_, a) => (a.v === null ? 'NULL' : a.v === undefined ? 'ABSENT' : a.v),
    },
    intArg: { type: GraphQLString, args: { v: { type: GraphQLInt } }, resolve: (_, a) => String(a.v) },
    floatArg: { type: GraphQLString, args: { v: { type: GraphQLFloat } }, resolve: (_, a) => String(a.v) },
    idArg: { type: GraphQLString, args: { v: { type: GraphQLID } }, resolve: (_, a) => String(a.v) },
    enumArg: { type: GraphQLString, args: { v: { type: Color } }, resolve: (_, a) => String(a.v) },
    inputArg: { type: GraphQLString, args: { v: { type: Inp } }, resolve: (_, a) => JSON.stringify(a.v) },
    listArg: { type: GraphQLString, args: { v: { type: new GraphQLList(GraphQLInt) } }, resolve: (_, a) => JSON.stringify(a.v) },
  },
});

const Mutation = new GraphQLObjectType({
  name: 'Mutation',
  fields: { bump: { type: new GraphQLNonNull(GraphQLString), resolve: () => 'bumped' } },
});

const schema = new GraphQLSchema({ query: Query, mutation: Mutation, types: [Thing, Other] });

const cases = JSON.parse(readFileSync(new URL('./cases.json', import.meta.url), 'utf8'));
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
