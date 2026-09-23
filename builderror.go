package graphql

import (
	"errors"
	"strconv"
	"strings"
)

// maxReportedBuildErrors is how many of a schema's build errors NewSchema
// renders. The rest are still returned and still walkable; they are simply not
// printed.
//
// A real 5 503-type schema produced 33 924 of them on first contact, one per
// line, which no terminal and no reader gets through -- and the first twenty
// were enough to act on every time, because the list is sorted, so a run of
// like errors sits together. The cap is deliberately not a summary by
// category: the two categories that made that schema's list enormous are now
// reported specifically and earlier (gqlc names every unbound scalar while
// generating, and an exported-name bug accounted for 342 of them), so
// classifying what is left would be machinery for a wall that no longer forms.
const maxReportedBuildErrors = 20

// buildError carries every schema build error and renders only the first few.
// It keeps errors.Join's contract -- Unwrap() []error -- so errors.Is, errors.As
// and a caller walking the list all behave as before.
type buildError struct{ errs []error }

func (e *buildError) Unwrap() []error { return e.errs }

func (e *buildError) Error() string {
	if len(e.errs) <= maxReportedBuildErrors {
		return errors.Join(e.errs...).Error()
	}
	var b strings.Builder
	b.WriteString("graphql: ")
	b.WriteString(strconv.Itoa(len(e.errs)))
	b.WriteString(" schema build errors; the first ")
	b.WriteString(strconv.Itoa(maxReportedBuildErrors))
	b.WriteString(" follow and the list is sorted, so the rest are mostly like them\n")
	for _, err := range e.errs[:maxReportedBuildErrors] {
		b.WriteString(err.Error())
		b.WriteByte('\n')
	}
	b.WriteString("... and ")
	b.WriteString(strconv.Itoa(len(e.errs) - maxReportedBuildErrors))
	b.WriteString(" more")
	return b.String()
}
