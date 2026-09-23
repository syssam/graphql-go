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

const (
	UserCount   = 100
	FriendCount = 2
)

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
