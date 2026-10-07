package repofmt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestCitedSymbolsExist pins the package-qualified symbol names this
// repository's prose points readers at.
//
// WHY IT EXISTS. TestEveryCitedRepoPathExists settled the file half of a
// citation: the path a document names has to be in the tree. The other half was
// unguarded, and it showed: docs/DESIGN.md and docs/RESUME.md wrote
// `config.EstimateCharsPerToken` for a field that lives on
// `config.QuotaConfig`, and docs/DESIGN.md wrote `Stats.Alerts` for a counter
// only `quota.Stats` has -- `internal/stats.Stats` has no such field. Both read
// as authoritative because the names around them were right.
//
// WHAT IT CAN HONESTLY CHECK. A backticked token is judged only when its first
// segment is the name of a package under internal/; anything else (a config key
// whose first segment happens to spell a package, an upstream library's
// symbol, a bare `Type.Member` that names no package) is left alone. The judged
// shapes are `pkg.Symbol`, `pkg.Method` for a method on any exported type in
// that package, and `pkg.Type.Member` for an exported field or method of that
// exported type. That is why `Stats.Alerts` above stays invisible: without a
// package prefix there is nothing to resolve against, and a bare `Stats` is
// ambiguous by construction. The prose that explains this boundary is in
// docs/ACCEPTANCE.md.
//
// WHY THE SURFACE IS THE PROSE. The path check's reasoning applies unchanged: a
// script or a comment may name a symbol it is about to define, and it is read
// next to the code that defines it. These surfaces are the ones a reader trusts
// as a description of the tree.
func TestCitedSymbolsExist(t *testing.T) {
	root := repoRoot(t)
	packages := internalPackages(t, root)

	cited := regexp.MustCompile("`([a-z][a-z0-9]*)\\.([A-Z][A-Za-z0-9]*)(?:\\.([A-Z][A-Za-z0-9]*))?`")

	total := 0
	var complaints []string
	for _, file := range proseSurfaces(t, root) {
		body := withoutFencedCodeBlocks(readFile(t, file))
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatalf("relativising %s: %v", file, err)
		}
		rel = filepath.ToSlash(rel)

		for _, m := range cited.FindAllStringSubmatchIndex(body, -1) {
			name := body[m[2]:m[3]]
			pkg, ok := packages[name]
			if !ok {
				continue
			}
			total++
			symbol := body[m[4]:m[5]]
			line := 1 + strings.Count(body[:m[0]], "\n")
			where := rel + ":" + strconv.Itoa(line)

			if !pkg.names[symbol] && !pkg.methods[symbol] {
				complaints = append(complaints, where+": cites `"+name+"."+symbol+
					"`, which internal/"+pkg.dir+" does not define; name an exported symbol of that package, or the file it lives in")
				continue
			}
			if m[6] < 0 {
				continue
			}
			member := body[m[6]:m[7]]
			if !pkg.types[symbol].members[member] {
				complaints = append(complaints, where+": cites `"+name+"."+symbol+"."+member+
					"`, which `"+name+"."+symbol+"` does not declare; name an exported field or method of that type")
			}
		}
	}

	// A check that matches nothing passes for the wrong reason, so the same
	// order-of-magnitude floor the other prose checks carry applies here. The
	// tree holds more than twice this today; it fires when the pattern or the
	// package index stops matching, not when a document is edited.
	t.Logf("checked %d package-qualified symbol citations against %d internal packages", total, len(packages))
	if total < 8 {
		t.Fatalf("found only %d symbol citations across %d surfaces; the pattern or the package index is wrong",
			total, len(packages))
	}
	sort.Strings(complaints)
	for _, c := range complaints {
		t.Errorf("%s", c)
	}
}

// symbolPackage is one package under internal/: the exported names a citation
// may use, and the exported members of each exported type.
type symbolPackage struct {
	dir     string
	names   map[string]bool
	methods map[string]bool
	types   map[string]symbolType
}

type symbolType struct {
	members map[string]bool
}

// internalPackages indexes internal/ by package name. Directories without
// non-test Go files are skipped, and a directory whose files disagree about
// their package clause is an error rather than a silent merge.
func internalPackages(t *testing.T, root string) map[string]symbolPackage {
	t.Helper()
	base := filepath.Join(root, "internal")
	packages := map[string]symbolPackage{}

	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		seen := ""
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(path, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing %s: %v", filepath.Join(path, name), err)
			}
			if seen == "" {
				seen = file.Name.Name
			} else if seen != file.Name.Name {
				t.Fatalf("%s holds both package %s and package %s", path, seen, file.Name.Name)
			}
			pkg := packages[seen]
			if pkg.names == nil {
				pkg = symbolPackage{
					dir:     filepath.ToSlash(strings.TrimPrefix(path, base+string(filepath.Separator))),
					names:   map[string]bool{},
					methods: map[string]bool{},
					types:   map[string]symbolType{},
				}
			}
			collectSymbols(pkg, file)
			packages[seen] = pkg
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", base, err)
	}
	return packages
}

// collectSymbols adds one parsed file's exported declarations to its package.
func collectSymbols(pkg symbolPackage, file *ast.File) {
	exported := func(name string) bool {
		return name != "" && ast.IsExported(name)
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !exported(d.Name.Name) {
				continue
			}
			if d.Recv == nil {
				pkg.names[d.Name.Name] = true
				continue
			}
			// A method is reachable as `pkg.Method` in prose even though Go
			// writes it on the type, so it is recorded in both places.
			pkg.methods[d.Name.Name] = true
			if recv := receiverName(d.Recv); recv != "" {
				entry := pkg.types[recv]
				if entry.members == nil {
					entry.members = map[string]bool{}
				}
				entry.members[d.Name.Name] = true
				pkg.types[recv] = entry
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !exported(s.Name.Name) {
						continue
					}
					pkg.names[s.Name.Name] = true
					entry := pkg.types[s.Name.Name]
					if entry.members == nil {
						entry.members = map[string]bool{}
					}
					switch body := s.Type.(type) {
					case *ast.StructType:
						for _, field := range body.Fields.List {
							if len(field.Names) == 0 {
								// An embedded type is a field named after the
								// type it embeds, and prose may name it that way.
								if name := typeName(field.Type); exported(name) {
									entry.members[name] = true
								}
								continue
							}
							for _, name := range field.Names {
								if exported(name.Name) {
									entry.members[name.Name] = true
								}
							}
						}
					case *ast.InterfaceType:
						for _, field := range body.Methods.List {
							if len(field.Names) == 0 {
								if name := typeName(field.Type); exported(name) {
									entry.members[name] = true
								}
								continue
							}
							for _, name := range field.Names {
								if exported(name.Name) {
									entry.members[name.Name] = true
								}
							}
						}
					}
					pkg.types[s.Name.Name] = entry
				case *ast.ValueSpec:
					for _, name := range s.Names {
						if exported(name.Name) {
							pkg.names[name.Name] = true
						}
					}
				}
			}
		}
	}
}

// receiverName is the bare type name a method is declared on, with all the
// wrappers Go allows around it (`*Cache`, `Cache[T]`) removed.
func receiverName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	return typeName(recv.List[0].Type)
}

func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.IndexExpr:
		return typeName(t.X)
	case *ast.IndexListExpr:
		return typeName(t.X)
	case *ast.ParenExpr:
		return typeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}
