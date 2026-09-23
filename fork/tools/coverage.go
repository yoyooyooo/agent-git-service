// Command coverage proves that generation construction preserved production Go
// bytes and the non-literal structure/imports of existing Go tests. It does not
// approve synthetic literal substitutions or replace running the tests.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

func git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"--no-replace-objects"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s failed", args[0])
	}
	return out, nil
}

func files(ref string) (map[string]string, error) {
	out, err := git("ls-tree", "-r", "-z", ref)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, row := range bytes.Split(out, []byte{0}) {
		if len(row) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(row, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 3 || fields[1] != "blob" {
			return nil, errors.New("unsupported tree leaf")
		}
		if strings.HasSuffix(string(path), ".go") {
			result[string(path)] = fields[2]
		}
	}
	return result, nil
}

type shape struct {
	Tokens  []string
	Imports []string
	Tests   []string
}

func sourceShape(path string, data []byte) (shape, error) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		return shape{}, errors.New("Go fixture parse failure")
	}
	result := shape{}
	for _, imp := range parsed.Imports {
		value, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return shape{}, errors.New("invalid fixture import")
		}
		result.Imports = append(result.Imports, value)
	}
	for _, declaration := range parsed.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if ok && fn.Recv == nil && (strings.HasPrefix(fn.Name.Name, "Test") || strings.HasPrefix(fn.Name.Name, "Fuzz") || strings.HasPrefix(fn.Name.Name, "Benchmark")) {
			result.Tests = append(result.Tests, fn.Name.Name)
		}
	}
	var scan scanner.Scanner
	var invalid bool
	scan.Init(token.NewFileSet().AddFile(path, -1, len(data)), data, func(token.Position, string) { invalid = true }, 0)
	for {
		_, tok, literal := scan.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.STRING {
			literal = "<string-literal>"
		}
		result.Tokens = append(result.Tokens, tok.String()+":"+literal)
	}
	if invalid {
		return shape{}, errors.New("Go fixture scan failure")
	}
	sort.Strings(result.Imports)
	sort.Strings(result.Tests)
	return result, nil
}

func sameStrings(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }

type report struct {
	Source               string   `json:"source"`
	Target               string   `json:"target"`
	ProductionFiles      int      `json:"production_files"`
	ProductionUnchanged  int      `json:"production_unchanged"`
	OriginalTestFiles    int      `json:"original_test_files"`
	OriginalTestEntries  int      `json:"original_test_entries"`
	TransformedTestFiles int      `json:"transformed_test_files"`
	AddedGoFiles         int      `json:"added_go_files"`
	Failures             []string `json:"failures"`
	Preserved            bool     `json:"preserved"`
	ClaimLimit           string   `json:"claim_limit"`
}

func compare(source, target string) (report, error) {
	before, err := files(source)
	if err != nil {
		return report{}, err
	}
	after, err := files(target)
	if err != nil {
		return report{}, err
	}
	result := report{Source: source, Target: target, Failures: []string{},
		ClaimLimit: "Production bytes and test structure only. Literal meaning, non-Go fixtures, APIs and runtime behavior require their own gates."}
	paths := make([]string, 0, len(before))
	for path := range before {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		oid := before[path]
		other, exists := after[path]
		if !strings.HasSuffix(path, "_test.go") {
			result.ProductionFiles++
			if exists && oid == other {
				result.ProductionUnchanged++
			} else {
				result.Failures = append(result.Failures, "production_changed_or_removed:"+path)
			}
			continue
		}
		result.OriginalTestFiles++
		if !exists {
			result.Failures = append(result.Failures, "test_file_removed:"+path)
			continue
		}
		data, err := git("cat-file", "blob", oid)
		if err != nil {
			return report{}, err
		}
		oldShape, err := sourceShape(path, data)
		if err != nil {
			return report{}, err
		}
		result.OriginalTestEntries += len(oldShape.Tests)
		if oid == other {
			continue
		}
		result.TransformedTestFiles++
		newData, err := git("cat-file", "blob", other)
		if err != nil {
			return report{}, err
		}
		newShape, err := sourceShape(path, newData)
		if err != nil {
			return report{}, err
		}
		if !sameStrings(oldShape.Tokens, newShape.Tokens) || !sameStrings(oldShape.Imports, newShape.Imports) || !sameStrings(oldShape.Tests, newShape.Tests) {
			result.Failures = append(result.Failures, "non_literal_test_change:"+path)
		}
	}
	for path := range after {
		if _, exists := before[path]; !exists {
			result.AddedGoFiles++
		}
	}
	result.Preserved = len(result.Failures) == 0
	return result, nil
}

func main() {
	source := flag.String("source", "", "exact reviewed input commit")
	target := flag.String("target", "HEAD", "exact reconstructed generation commit")
	flag.Parse()
	if *source == "" || strings.HasPrefix(*source, "-") || strings.HasPrefix(*target, "-") {
		fmt.Fprintln(os.Stderr, "explicit source and target references are required")
		os.Exit(2)
	}
	result, err := compare(*source, *target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(result)
	if !result.Preserved {
		os.Exit(1)
	}
}
