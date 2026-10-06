// Package data builds the fixture both engines execute against, so a
// difference between them is the engine and not the input.
package data

import "strconv"

// User is the shared model for both graphql-go and gqlgen so the two
// engines marshal the same Go values.
type User struct {
	ID      string
	Name    string
	Email   string
	Friends []*User
}

// The fixture's size: UserCount users with FriendCount friends each.
const (
	UserCount   = 100
	FriendCount = 2
)

// Dataset is built from indexes alone, so every call, and so both engines,
// get the same graph: user i befriends the FriendCount users after it.
func Dataset() []*User {
	users := make([]*User, UserCount)
	for i := range users {
		id := strconv.Itoa(i + 1)
		users[i] = &User{
			ID:    id,
			Name:  "User " + id,
			Email: "user" + id + "@example.com",
		}
	}
	for i, u := range users {
		u.Friends = make([]*User, FriendCount)
		for j := range FriendCount {
			u.Friends[j] = users[(i+j+1)%UserCount]
		}
	}
	return users
}
