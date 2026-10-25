package calculation

// InputValueMapSQL lets the external tests explain LoadInputValueMap's plan.
const InputValueMapSQL = inputValueMapSQL

// SetCopyRowsAt makes result sets of n rows or more go through COPY, until
// the returned func restores the default.
func SetCopyRowsAt(n int) (restore func()) {
	old := copyRowsAt
	copyRowsAt = n
	return func() { copyRowsAt = old }
}

// SetLeanCellsAt makes metrics of n cells or more take the lean path,
// until the returned func restores the default.
func SetLeanCellsAt(n int64) (restore func()) {
	old := leanCellsAt
	leanCellsAt = n
	return func() { leanCellsAt = old }
}
