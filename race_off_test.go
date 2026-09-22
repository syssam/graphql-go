//go:build !race

package graphql

// raceEnabled reports that this binary was built with -race, which changes
// allocation counts. The allocation baseline is meaningless under it.
const raceEnabled = false
