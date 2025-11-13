package wwwauth

import (
	"fmt"
	"log/slog"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
	"github.com/authenticvision/util-go/logutil"
)

var lex = lexer.MustSimple([]lexer.SimpleRule{
	{Name: "Whitespace", Pattern: `\s+`},
	{Name: "FieldSep", Pattern: `,`},
	{Name: "ValueSep", Pattern: `=`},
	{Name: "Name", Pattern: `[a-zA-Z0-9_*.-]+`},
	{Name: "Value", Pattern: `"(\\"|[^"])*"`},
})

type wwwauth struct {
	Scheme string  `parser:"@Name"`
	Params []param `parser:"@@ (',' @@)*"`
}

type param struct {
	Field string `parser:"@Name '='"`
	Value string `parser:"@Value"`
}

var parser = participle.MustBuild[wwwauth](
	participle.Lexer(lex),
	participle.Elide("Whitespace"),
	participle.Unquote("Value"),
)

type WWWAuthenticate struct {
	Realm   string
	Service string
	Scope   string
}

type Error struct {
	Code        string
	Description string
	URI         string
}

func (e Error) Error() string {
	return fmt.Sprintf("%s (%s)", e.Code, e.Description)
}

func Parse(s string) (WWWAuthenticate, error) {
	parsed, err := parser.ParseString("", s)
	if err != nil {
		return WWWAuthenticate{}, logutil.NewError(err, "parse")
	}
	var ret WWWAuthenticate
	var wwwErr Error
	for _, p := range parsed.Params {
		switch p.Field {
		case "realm":
			ret.Realm = p.Value
		case "service":
			ret.Service = p.Value
		case "scope":
			ret.Scope = p.Value
		case "error":
			wwwErr.Code = p.Value
		case "error_description":
			wwwErr.Description = p.Value
		case "error_uri":
			wwwErr.URI = p.Value
		default:
			return WWWAuthenticate{}, logutil.NewError(nil, "unknown field", slog.String("field", p.Field))
		}
	}
	if wwwErr.Code != "" {
		return WWWAuthenticate{}, wwwErr
	}
	return ret, nil
}
