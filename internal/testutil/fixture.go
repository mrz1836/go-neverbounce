package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Fixture returns the contents of the file name, a slash-separated path under
// testdata, failing tb when it cannot be read.
func Fixture(tb testing.TB, name string) []byte {
	tb.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
	require.NoError(tb, err)
	return data
}
