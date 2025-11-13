package wwwauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)
	wwwauth := `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/ubuntu:pull"`
	parsed, err := Parse(wwwauth)
	r.NoError(err)
	a.Equal(parsed.Realm, "https://auth.docker.io/token")
	a.Equal(parsed.Service, "registry.docker.io")
	a.Equal(parsed.Scope, "repository:library/ubuntu:pull")
}
