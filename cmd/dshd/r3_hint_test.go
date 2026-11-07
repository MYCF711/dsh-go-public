package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R3 红线：工具的每个 error 分支都必须设 Hint——模型下一句就靠这句话。
//
// 这条一直靠"写的时候记得"，加上各工具零散的测试。没有一次全仓扫描，
// 所以"新写的工具漏了 Hint"和"所有工具都合规"在门禁里长得一模一样。
//
// 判据（满足其一即可）：
//  1. 直接返回 `spi.Result{Hint: ...}` 这种带 Hint 的字面量
//  2. 返回某个"东西"（res / call.Result …），且在同一层里、这条 return **之前**
//     刚给它赋过 .Hint
//
// 这个形状是第三版。前两版都被实测否掉：
//   - 只看 Exec：watch 的 Hint 写在私有函数里，漏掉一大半
//   - "整个函数里赋过就算"：pipeline 六条分支共用一个 call.Result，
//     删掉其中一条的 Hint 照样绿
//
// 扫的范围是**所有 `(spi.Result, error)` 函数**，不只是 `Exec`。
func TestEveryToolErrorHasHint(t *testing.T) {
	fset := token.NewFileSet()
	var missing []string
	files, points := 0, 0

	root := filepath.Join("..", "..", "modules")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // 解析不了的文件交给 go build 去报
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !returnsResultAndError(fd) {
				return true
			}
			missing = append(missing, scanFunc(fset, fd.Body)...)
			points += countErrorReturns(fd.Body)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("扫 modules 失败: %v", err)
	}
	if files == 0 {
		t.Fatal("一个工具文件都没扫到——路径写错了，这条测试等于没跑")
	}
	if len(missing) > 0 {
		t.Fatalf("R3 被违反：%d 处返回 error 却没在紧邻的位置设 Hint: %s",
			len(missing), strings.Join(missing, " "))
	}
	t.Logf("扫了 %d 个工具文件、%d 个返回 error 的点，每个都带 Hint", files, points)
}

// scanFunc 逐条 return 检查。用"同一层、这条 return 之前"来配对，
// 是因为本仓所有这类代码都是这么写的：先 `x.Hint = ...`，下一行 `return x, err`。
func scanFunc(fset *token.FileSet, body *ast.BlockStmt) []string {
	var out []string
	var walk func(block *ast.BlockStmt)
	walk = func(block *ast.BlockStmt) {
		for i, stmt := range block.List {
			if ret, ok := stmt.(*ast.ReturnStmt); ok && returnsError(ret) {
				ok2 := hasHintLiteral(ret.Results[0]) ||
					hintedBefore(block.List[:i], ret.Results[0]) ||
					hintedAnywhere(body, ret.Results[0])
				if !ok2 {
					out = append(out, fset.Position(ret.Pos()).String())
				}
				continue
			}
			// 进子块：if/for/switch 里的分支各自算一层
			ast.Inspect(stmt, func(n ast.Node) bool {
				if b, ok := n.(*ast.BlockStmt); ok && b != block {
					walk(b)
					return false
				}
				return true
			})
		}
	}
	walk(body)
	return out
}

// hintedAnywhere 是整个函数范围内的兜底：有的地方 Hint 在更早的 if/else 里设、
// 或者这个函数返回的是"一路累积下来"的 result（pipeline 的末尾就是）。
//
// **已知盲点**：多条分支共用一个 result 变量时，删掉其中一条分支的 Hint
// 这条扫描看不出来（其余分支的赋值让它通过了）。
// 明知不完美也留着，是因为更严的那版会对正确代码报警——
// 一个总在误报的守卫，最后一定会被人加豁免，然后没人再看它。
func hintedAnywhere(body *ast.BlockStmt, target ast.Expr) bool {
	want := exprPath(target)
	if want == "" {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "Hint" && exprPath(sel.X) == want {
				found = true
			}
		}
		return true
	})
	return found
}

// hintedBefore 看这条 return 之前、同一层里有没有给同一个东西赋 .Hint。
func hintedBefore(before []ast.Stmt, target ast.Expr) bool {
	want := exprPath(target)
	if want == "" {
		return false
	}
	for _, st := range before {
		as, ok := st.(*ast.AssignStmt)
		if !ok {
			continue
		}
		for _, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Hint" {
				continue
			}
			if exprPath(sel.X) == want {
				return true
			}
		}
	}
	return false
}

func returnsError(ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 2 {
		return false
	}
	id, ok := ret.Results[1].(*ast.Ident)
	return !ok || id.Name != "nil"
}

func countErrorReturns(body *ast.BlockStmt) int {
	n := 0
	ast.Inspect(body, func(m ast.Node) bool {
		if ret, ok := m.(*ast.ReturnStmt); ok && returnsError(ret) {
			n++
		}
		return true
	})
	return n
}

func hasHintLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Hint" {
			return true
		}
	}
	return false
}

// exprPath 把 a、a.B、a.B.C 渲染成同一个字符串，用来比较"给谁赋了 Hint"
// 与 "返回的是谁"。认不出的形状返回空串。
func exprPath(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if base := exprPath(t.X); base != "" {
			return base + "." + t.Sel.Name
		}
	}
	return ""
}

// returnsResultAndError 判断这个函数是不是 `(spi.Result, error)` 形状。
func returnsResultAndError(fd *ast.FuncDecl) bool {
	if fd.Type.Results == nil || len(fd.Type.Results.List) != 2 {
		return false
	}
	isResult := func(e ast.Expr) bool {
		switch t := e.(type) {
		case *ast.SelectorExpr:
			return t.Sel.Name == "Result"
		case *ast.Ident:
			return t.Name == "Result"
		}
		return false
	}
	id, ok := fd.Type.Results.List[1].Type.(*ast.Ident)
	return isResult(fd.Type.Results.List[0].Type) && ok && id.Name == "error"
}
