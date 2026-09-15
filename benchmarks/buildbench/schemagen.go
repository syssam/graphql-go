// Package buildbench generates synthetic schemas and measures what they cost
// to generate and compile, for graphql-go's gqlc against gqlgen.
//
// The runtime comparison lives in the parent package. This one exists to test
// the premise the project is built on: that gqlgen's build pipeline, not its
// executor, is what breaks down on large schemas.
package buildbench

import (
	"fmt"
	"strings"
)

// Prelude holds the shared types and the root operation types every entity
// extends. gqlc groups generated code by SDL file stem, so what lives in
// which file decides how the output is packaged.
const Prelude = `scalar Time

interface Node {
  id: ID!
}

enum OrderDirection {
  ASC
  DESC
}

type PageInfo {
  hasNextPage: Boolean!
  hasPreviousPage: Boolean!
  startCursor: String
  endCursor: String
}

type Query {
  node(id: ID!): Node
}

type Mutation {
  ping: Boolean!
}
`

// SDL returns the whole schema as one document. With a single SDL file gqlc
// puts every type in one group, which is the flat layout.
func SDL(n int) string {
	var b strings.Builder
	b.WriteString(Prelude)
	for i := range n {
		b.WriteString(entityBlock(i, n))
	}
	return b.String()
}

// SDLFiles returns the same schema split into one file per entity plus a
// prelude, which is the layout that makes gqlc emit one package per entity.
func SDLFiles(n int) map[string]string {
	out := make(map[string]string, n+1)
	out["prelude.graphql"] = Prelude
	for i := range n {
		out[strings.ToLower(EntityName(i))+".graphql"] = entityBlock(i, n)
	}
	return out
}

// entityBlock is one entity's types plus its slice of the root operations,
// written as extensions so each entity owns its own file.
func entityBlock(i, n int) string {
	if n < 1 {
		panic("buildbench: n must be >= 1")
	}
	name := EntityName(i)
	owner := EntityName((i + 1) % n)
	child := EntityName((i + 2) % n)
	lower := lowerFirst(name)

	return fmt.Sprintf(`
type %[1]s implements Node {
  id: ID!
  name: String!
  description: String
  createdAt: Time!
  updatedAt: Time
  active: Boolean!
  score: Int!
  ratio: Float!
  tags: [String!]!
  owner: %[2]s
  children(first: Int, after: String, last: Int, before: String, where: %[3]sWhereInput, orderBy: %[3]sOrder): %[3]sConnection!
}

type %[1]sConnection {
  edges: [%[1]sEdge!]!
  pageInfo: PageInfo!
  totalCount: Int!
}

type %[1]sEdge {
  node: %[1]s!
  cursor: String!
}

input %[1]sWhereInput {
  id: ID
  idIn: [ID!]
  name: String
  nameContains: String
  nameHasPrefix: String
  active: Boolean
  scoreGT: Int
  scoreLT: Int
  createdAtGT: Time
  and: [%[1]sWhereInput!]
  or: [%[1]sWhereInput!]
  not: %[1]sWhereInput
}

enum %[1]sOrderField {
  NAME
  CREATED_AT
  SCORE
}

input %[1]sOrder {
  field: %[1]sOrderField!
  direction: OrderDirection!
}

input Create%[1]sInput {
  name: String!
  description: String
  active: Boolean
  score: Int
  ratio: Float
  tags: [String!]
  ownerID: ID
}

input Update%[1]sInput {
  name: String
  description: String
  active: Boolean
  score: Int
  ratio: Float
  tags: [String!]
  ownerID: ID
}

extend type Query {
  %[4]s(id: ID!): %[1]s
  %[4]ss(first: Int, after: String, last: Int, before: String, where: %[1]sWhereInput, orderBy: %[1]sOrder): %[1]sConnection!
}

extend type Mutation {
  create%[1]s(input: Create%[1]sInput!): %[1]s!
  update%[1]s(id: ID!, input: Update%[1]sInput!): %[1]s!
  delete%[1]s(id: ID!): Boolean!
}
`, name, owner, child, lower)
}

// EntityName is the GraphQL type name of the i-th entity.
func EntityName(i int) string { return fmt.Sprintf("Entity%03d", i) }

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
