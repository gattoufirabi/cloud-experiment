package web

// percentOf is used by the template for bar widths.
func percentOf(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	return int(part * 100 / whole)
}
