package internal_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const mod = "github.com/MiguelSanzBr/webhook-relay/internal/"

// Domínio e aplicação não conhecem HTTP, adapter nem I/O. Precisou de algo de
// fora, vira port na aplicação e o adapter implementa.
var allowed = map[string][]string{
	"domain": {"crypto/hmac", "crypto/sha256", "encoding/hex", "errors", "math/rand/v2", "strconv", "strings", "time"},
	"app":    {"context", "errors", "time", mod + "domain"},
}

func TestDependencyRule(t *testing.T) {
	for dir, ok := range allowed {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		code := 0
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			code++
			parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range parsed.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if !slices.Contains(ok, path) {
					t.Errorf("%s importa %s, fora do permitido em internal/%s", f, path, dir)
				}
			}
		}
		// pasta sumida ou vazia passaria sem checar nada
		if code == 0 {
			t.Errorf("internal/%s sem código pra checar", dir)
		}
	}
}
