package client

import "strconv"

// formatInt is the tiny indirection so resource files can do
// `c.Get(ctx, "/v1/foo/"+itoa(id), ...)` without each one importing
// strconv directly.
func formatInt(i int) string {
	return strconv.Itoa(i)
}
