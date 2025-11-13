package cache

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPathSanitize(t *testing.T) {
	require.Equal(t, "/test/asdf", filepath.Join("/test", filepath.Join("/", "../../asdf")))
}
