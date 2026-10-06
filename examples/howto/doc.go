// Package howto is the code behind the task pages in docs/tasks: one file and
// its test per page.
//
//	docs/tasks/n-plus-one.md        loader.go, loader_test.go
//	docs/tasks/authorization.md     authz.go,  authz_test.go
//	docs/tasks/custom-scalar.md     scalar.go, scalar_test.go
//	docs/tasks/testing-a-server.md  server.go, server_test.go
//
// A page shows nothing these files do not contain. pages_test.go reads every
// Go block out of every page and fails unless it finds the block here, so a
// refactor that changes the code and not the page is a failing test rather
// than a page that quietly stopped being true.
//
// The four files share a package and nothing else: each page is meant to be
// read alone, so none of them reuses another's types.
package howto
