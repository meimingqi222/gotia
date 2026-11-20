package main

import (
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// fileChange is how a Go file changed, ignoring comments and layout.
type fileChange struct {
	changed []string // functions that changed or disappeared
	added   []string // functions only the new version has
	names   []string // package-level vars and consts whose declaration changed
	// fallback, when set, is a change coverage cannot attribute to functions
	// or names: the whole package and its dependents are affected.
	fallback string
}

func diffGoFile(before, after []byte) (fileChange, error) {
	var fc fileChange
	old, err := summarize(before)
	if err != nil {
		return fc, err
	}
	cur, err := summarize(after)
	if err != nil {
		return fc, err
	}
	switch {
	case before != nil && after != nil && old.pkg != cur.pkg:
		fc.fallback = "package clause changed"
	case old.directives != cur.directives:
		fc.fallback = "a //go: directive or build constraint changed"
	case old.special != cur.special:
		fc.fallback = "a blank, dot or side-effect declaration changed"
	}
	for name, p := range cur.imports {
		if q, ok := old.imports[name]; ok && p != q && fc.fallback == "" {
			fc.fallback = "import " + name + " now names another package"
		}
	}
	for name, o := range old.types {
		if c, ok := cur.types[name]; ok && c != o && fc.fallback == "" {
			fc.fallback = "type " + name + " changed"
		}
	}
	implicit := func(key string) bool { return key == "init" || strings.HasPrefix(key, "(") }
	for key, o := range old.funcs {
		c, ok := cur.funcs[key]
		if ok && c.body == o.body {
			continue
		}
		fc.changed = append(fc.changed, key)
		if implicit(key) && fc.fallback == "" {
			// A removed method or a new signature can change which interfaces
			// a type satisfies; init runs without a caller.
			if key == "init" {
				fc.fallback = "init changed"
			} else if !ok || c.sig != o.sig {
				fc.fallback = "method " + key + " removed or its signature changed"
			}
		}
	}
	for key := range cur.funcs {
		if _, ok := old.funcs[key]; !ok {
			fc.added = append(fc.added, key)
			if implicit(key) && fc.fallback == "" {
				fc.fallback = "added " + key + ", which may satisfy an interface or run at init"
			}
		}
	}
	for name, o := range old.values {
		if c, ok := cur.values[name]; !ok || c != o {
			fc.names = append(fc.names, name)
		}
	}
	for name := range cur.values {
		if _, ok := old.values[name]; !ok {
			fc.names = append(fc.names, name)
		}
	}
	sort.Strings(fc.changed)
	sort.Strings(fc.added)
	sort.Strings(fc.names)
	return fc, nil
}

type funcText struct{ body, sig string }

type fileSummary struct {
	pkg        string
	funcs      map[string]funcText
	values     map[string]string // var and const name → declaration tokens
	types      map[string]string
	imports    map[string]string // local name → import path
	directives string
	special    string
}

func summarize(src []byte) (*fileSummary, error) {
	s := &fileSummary{funcs: map[string]funcText{}, values: map[string]string{}, types: map[string]string{}, imports: map[string]string{}}
	if src == nil {
		return s, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	s.pkg = f.Name.Name
	var directives, special strings.Builder
	for _, group := range f.Comments {
		for _, c := range group.List {
			if strings.HasPrefix(c.Text, "//go:") || strings.HasPrefix(c.Text, "// +build") {
				directives.WriteString(c.Text + "\n")
			}
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			key := funcKey(d)
			text := s.funcs[key] // several init functions share one key
			text.body += tokens(fset, src, d)
			text.sig += tokens(fset, src, d.Type)
			s.funcs[key] = text
		case *ast.GenDecl:
			switch d.Tok {
			case token.IMPORT:
				for _, spec := range d.Specs {
					is := spec.(*ast.ImportSpec)
					p, _ := strconv.Unquote(is.Path.Value)
					name := path.Base(p)
					if is.Name != nil {
						name = is.Name.Name
					}
					if name == "_" || name == "." {
						special.WriteString(name + " " + p + "\n")
					} else {
						s.imports[name] = p
					}
				}
			case token.TYPE:
				for _, spec := range d.Specs {
					ts := spec.(*ast.TypeSpec)
					s.types[ts.Name.Name] = tokens(fset, src, ts)
				}
			default: // var, const
				// In a const group, iota ties every value to its position, so
				// the group is one declaration for all of its names.
				group := d.Tok == token.CONST && d.Lparen.IsValid()
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					text := tokens(fset, src, vs)
					if group {
						text = tokens(fset, src, d)
					}
					for _, name := range vs.Names {
						if name.Name == "_" {
							special.WriteString(text + "\n")
						} else {
							s.values[name.Name] = text
						}
					}
				}
			}
		}
	}
	s.directives, s.special = directives.String(), special.String()
	return s, nil
}

// tokens renders a node's source as its token stream, without comments or
// layout.
func tokens(fset *token.FileSet, src []byte, n ast.Node) string {
	file := fset.File(n.Pos())
	text := src[file.Offset(n.Pos()):file.Offset(n.End())]
	var s scanner.Scanner
	s.Init(token.NewFileSet().AddFile("", -1, len(text)), text, nil, 0)
	var b strings.Builder
	semicolon := false
	for {
		_, tok, lit := s.Scan()
		// A semicolon right before a closing bracket is layout: Go inserts one
		// at a line end, and "{ return x }" on one line has none. Elsewhere it
		// separates statements ("return\nf()" is not "return f()").
		if semicolon && tok != token.RBRACE && tok != token.RPAREN {
			b.WriteString("; ")
		}
		semicolon = tok == token.SEMICOLON
		switch {
		case tok == token.EOF:
			return b.String()
		case !semicolon:
			b.WriteString(tok.String() + lit + " ")
		}
	}
}

// funcKey names a function the way gotia reports it: Name or (*T).Name.
func funcKey(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	recv, star := d.Recv.List[0].Type, ""
	if s, ok := recv.(*ast.StarExpr); ok {
		recv, star = s.X, "*"
	}
	switch x := recv.(type) {
	case *ast.IndexExpr:
		recv = x.X
	case *ast.IndexListExpr:
		recv = x.X
	}
	name := "?"
	if id, ok := recv.(*ast.Ident); ok {
		name = id.Name
	}
	return "(" + star + name + ")." + d.Name.Name
}

// isTestFunc reports whether go test runs name as a top-level test, example
// or fuzz target.
func isTestFunc(name string) bool {
	for _, prefix := range []string{"Test", "Example", "Fuzz"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok && name != "TestMain" {
			r, _ := utf8.DecodeRuneInString(rest)
			return rest == "" || !unicode.IsLower(r)
		}
	}
	return false
}
