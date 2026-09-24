// graphql-js itself, behind the thinnest spec-compliant server there is.
import { createHandler } from 'graphql-http/lib/use/http';
import { createServer } from 'node:http';
import { makeExecutableSchema } from '@graphql-tools/schema';
import { typeDefs, dataset, resolvers } from './schema.mjs';

const users = dataset();
const schema = makeExecutableSchema({ typeDefs, resolvers: resolvers(users) });
const port = Number(process.env.PORT || 18097);
createServer(createHandler({ schema })).listen(port, '0.0.0.0', () =>
  console.log('graphql-http listening on ' + port));
