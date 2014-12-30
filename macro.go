// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU General
// Public License for more details.
//
// You should have received a copy of the GNU General Public License along
// with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"log"
	"os"
	"strings"
)

const prefix = "MACRO_"

var recursive = flag.Bool("r", false, "Expand macros recursively")

type visitor struct {
	macros       map[string]*ast.BlockStmt // macro definitions indexed by name
	macroParams  map[string][]string       // lists of the names of the macros parameters
	currentMacro string                    // the name of the macro we are currently expanding
	replace      []ast.Expr                // parameters of the macro we are currently expanding
	blocks       []*ast.BlockStmt          // a stack of nested code blocks
	level        int                       // nesting level
	lists        [][]ast.Stmt              // a stack of lists of expanded statements
}

func (v *visitor) transformBasicLit(lit *ast.BasicLit) ast.Expr {
	return &ast.BasicLit{
		ValuePos: token.NoPos,
		Kind:     lit.Kind,
		Value:    lit.Value,
	}
}

func (v *visitor) transformIdent(ident *ast.Ident) ast.Expr {
	params := v.macroParams[v.currentMacro]
	for i, param := range params {
		if param == ident.Name {
			return v.replace[i]
		}
	}

	return &ast.Ident{
		NamePos: token.NoPos,
		Name:    ident.Name,
		Obj:     ident.Obj,
	}
}

func (v *visitor) transformBinaryExpr(expr *ast.BinaryExpr) ast.Expr {
	return &ast.BinaryExpr{
		X:     v.transformExpr(expr.X),
		OpPos: token.NoPos,
		Op:    expr.Op,
		Y:     v.transformExpr(expr.Y),
	}
}

func (v *visitor) transformIndexExpr(expr *ast.IndexExpr) ast.Expr {
	return &ast.IndexExpr{
		X:      v.transformExpr(expr.X),
		Lbrack: token.NoPos,
		Index:  v.transformExpr(expr.Index),
		Rbrack: token.NoPos,
	}
}

func (v *visitor) transformUnaryExpr(expr *ast.UnaryExpr) ast.Expr {
	return &ast.UnaryExpr{
		OpPos: token.NoPos,
		Op:    expr.Op,
		X:     v.transformExpr(expr.X),
	}
}

func (v *visitor) transformCallExpr(expr *ast.CallExpr) ast.Expr {
	args := make([]ast.Expr, len(expr.Args))
	for i := 0; i < len(args); i++ {
		args[i] = v.transformExpr(expr.Args[i])
	}
	return &ast.CallExpr{
		Fun:      v.transformExpr(expr.Fun),
		Lparen:   token.NoPos,
		Args:     args,
		Ellipsis: token.NoPos,
		Rparen:   token.NoPos,
	}
}

func (v *visitor) transformParenExpr(expr *ast.ParenExpr) ast.Expr {
	return &ast.ParenExpr{
		Lparen: token.NoPos,
		X:      v.transformExpr(expr.X),
		Rparen: token.NoPos,
	}
}

func (v *visitor) transformExpr(expr ast.Expr) ast.Expr {
	switch expr := expr.(type) {
	case *ast.Ident:
		return v.transformIdent(expr)
	case *ast.BinaryExpr:
		return v.transformBinaryExpr(expr)
	case *ast.BasicLit:
		return v.transformBasicLit(expr)
	case *ast.IndexExpr:
		return v.transformIndexExpr(expr)
	case *ast.UnaryExpr:
		return v.transformUnaryExpr(expr)
	case *ast.CallExpr:
		return v.transformCallExpr(expr)
	case *ast.ParenExpr:
		return v.transformParenExpr(expr)
	default:
		panic(fmt.Sprintf("unexpected type: %T", expr))
	}
}

func (v *visitor) transformAssignStmt(stmt *ast.AssignStmt) ast.Stmt {
	lhs := make([]ast.Expr, len(stmt.Lhs))
	for i, expr := range stmt.Lhs {
		lhs[i] = v.transformExpr(expr)
	}

	rhs := make([]ast.Expr, len(stmt.Rhs))
	for i, expr := range stmt.Rhs {
		rhs[i] = v.transformExpr(expr)
	}

	return &ast.AssignStmt{
		Lhs:    lhs,
		TokPos: token.NoPos,
		Tok:    stmt.Tok,
		Rhs:    rhs,
	}
}

func (v *visitor) transformExprStmt(stmt *ast.ExprStmt) ast.Stmt {
	return &ast.ExprStmt{X: v.transformExpr(stmt.X)}
}

func (v *visitor) Expand(block *ast.BlockStmt) {
	i := len(v.lists) - 1
	v.lists[i] = make([]ast.Stmt, len(block.List))
	for j, stmt := range block.List {
		switch stmt := stmt.(type) {
		case *ast.AssignStmt:
			v.lists[i][j] = v.transformAssignStmt(stmt)
		case *ast.ExprStmt:
			v.lists[i][j] = v.transformExprStmt(stmt)
		default:
			panic(fmt.Sprintf("unexpected type: %T", stmt))
		}
	}
}

func (v *visitor) processBlock(stmt *ast.BlockStmt) {
	v.blocks = append(v.blocks, stmt)
	// Increase nesting level.
	v.level++
	stmts := make([]ast.Stmt, 0)

	// Walk all the statements.
	for _, stmt := range stmt.List {
		v.lists = append(v.lists, nil)

		ast.Walk(v, stmt)

		i := len(v.lists) - 1
		if v.lists[i] == nil {
			// No macro usages in this statement,
			// keep the original statement.
			stmts = append(stmts, stmt)
		} else {
			// Replace the statement with an expanded
			// list of statements.
			stmts = append(stmts, v.lists[i]...)
		}

		v.lists = v.lists[:i]
	}

	// Decrease nesting level.
	v.level--
	// Replace the list of the block statements with a new (expanded) list.
	v.blocks[v.level].List = stmts
	v.blocks = v.blocks[:v.level]
}

func (v *visitor) Visit(node ast.Node) ast.Visitor {
	switch node := node.(type) {
	case *ast.FuncDecl:
		// A function declaration.
		name := node.Name.Name

		// Check if it is a macro definition.
		if !strings.HasPrefix(name, prefix) {
			break
		}

		// Strip the MACRO_ prefix from the name.
		name = strings.TrimPrefix(name, prefix)

		if *recursive {
			// Recursively expand macros.
			v.processBlock(node.Body)
		}

		// Save the macro body for later use.
		v.macros[name] = node.Body

		// Save the macro params names.
		var params []string
		for _, p := range node.Type.Params.List {
			for _, ident := range p.Names {
				params = append(params, ident.Name)
			}
		}
		v.macroParams[name] = params

		return nil

	case *ast.BlockStmt:
		// A code block.
		v.processBlock(node)

		return nil

	case *ast.CallExpr:
		// A function call.
		// Check if it is a macro call.
		if ident, ok := node.Fun.(*ast.Ident); ok {
			if repl, ok := v.macros[ident.Name]; ok {
				v.currentMacro = ident.Name

				// Prepare a list of parameter substitutions.
				v.replace = make([]ast.Expr, len(node.Args))
				for i, a := range node.Args {
					v.replace[i] = a
				}

				// Expand this macro call.
				v.Expand(repl)

				return nil
			}
		}
	}

	return v
}

func main() {
	log.SetFlags(0) // no date and time
	flag.Parse()

	if len(flag.Args()) != 2 {
		log.Fatal("Usage: macro [-r] input.go.tmpl output.go")
	}

	// Parse the template.
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, flag.Arg(0), nil, parser.ParseComments)
	if err != nil {
		log.Fatal(err)
	}

	// Walk and transform the AST tree.
	v := visitor{
		macros:      make(map[string]*ast.BlockStmt),
		macroParams: make(map[string][]string),
	}
	ast.Walk(&v, tree)

	// Remove macro definitions.
	decls := make([]ast.Decl, 0)
	for _, decl := range tree.Decls {
		if decl, ok := decl.(*ast.FuncDecl); ok {
			if strings.HasPrefix(decl.Name.Name, prefix) {
				continue
			}
		}
		decls = append(decls, decl)
	}
	tree.Decls = decls

	// Write the formatted result.
	f, err := os.OpenFile(flag.Arg(1), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	format.Node(f, fset, tree)
}
