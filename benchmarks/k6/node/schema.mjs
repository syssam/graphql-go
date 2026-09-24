// The benchmark fixture, matching benchmarks/internal/data exactly: 100 users,
// 2 friends each, wrapping by (i+j+1) % 100. A difference here would be the
// dataset, not the engine.
export const typeDefs = `
  type User { id: ID! name: String! email: String! friends: [User!]! }
  type Query { users: [User!]! }
`;

export const UserCount = 100;
export const FriendCount = 2;

export function dataset() {
  const users = [];
  for (let i = 0; i < UserCount; i++) {
    const id = String(i + 1);
    users.push({ id, name: 'User ' + id, email: 'user' + id + '@example.com', friends: [] });
  }
  for (let i = 0; i < UserCount; i++) {
    for (let j = 0; j < FriendCount; j++) {
      users[i].friends.push(users[(i + j + 1) % UserCount]);
    }
  }
  return users;
}

// Trivial resolvers on both sides: the field values are already on the object,
// so what is measured is the engine walking the plan, not user code.
export function resolvers(users) {
  return { Query: { users: () => users } };
}
