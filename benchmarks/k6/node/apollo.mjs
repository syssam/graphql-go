import { ApolloServer } from '@apollo/server';
import { startStandaloneServer } from '@apollo/server/standalone';
import { typeDefs, dataset, resolvers } from './schema.mjs';

const users = dataset();
const server = new ApolloServer({
  typeDefs,
  resolvers: resolvers(users),
  introspection: false,
});
const port = Number(process.env.PORT || 18096);
await startStandaloneServer(server, { listen: { port, host: '0.0.0.0' } });
console.log('apollo listening on ' + port);
