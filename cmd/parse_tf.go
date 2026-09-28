package cmd

import (
	"errors"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	"github.com/zclconf/go-cty/cty"
)

var localModuleSourcePrefixes = []string{
	"./",
	"../",
	".\\",
	"..\\",
}

// rootLocals lazily returns the locals available to a root module's module sources.
type rootLocals func() map[string]cty.Value

// parseTerraformLocalModuleSource returns globs for the local modules the module at path
// calls, recursively. locals is nil for a child module: a local never crosses a module
// boundary.
func parseTerraformLocalModuleSource(path string, locals rootLocals) ([]string, error) {
	module, diags := tfconfig.LoadModule(path)
	// modules, diags := parser.loadConfigDir(path)
	if diags.HasErrors() {
		return nil, errors.New(diags.Error())
	}

	var sourceMap = map[string]bool{}
	for _, mc := range module.ModuleCalls {
		source := mc.Source
		if !isLocalTerraformModuleSource(source) && locals != nil {
			source = evalLocalModuleSource(source, locals)
		}
		if isLocalTerraformModuleSource(source) {
			modulePath := joinPath(path, source)
			modulePathGlob := joinPath(modulePath, "*.tf*")

			if _, exists := sourceMap[modulePathGlob]; exists {
				continue
			}
			sourceMap[modulePathGlob] = true

			// find local module source recursively
			subSources, err := parseTerraformLocalModuleSource(modulePath, nil)
			if err != nil {
				return nil, err
			}

			for _, subSource := range subSources {
				sourceMap[subSource] = true
			}
		}
	}

	var sources = []string{}
	for source := range sourceMap {
		sources = append(sources, source)
	}

	return sources, nil
}

// evalLocalModuleSource evaluates a module source that references locals.
// tfconfig reports such a source as its raw expression text. Anything that does
// not evaluate to a known string yields "".
func evalLocalModuleSource(raw string, locals rootLocals) string {
	expr, diags := hclsyntax.ParseExpression([]byte(raw), "", hcl.InitialPos)
	if diags.HasErrors() {
		return ""
	}
	traversals := expr.Variables()
	if len(traversals) == 0 {
		return ""
	}
	// A registry source such as hashicorp/consul/aws parses as a division of
	// variables, so only local.* references are evaluated.
	for _, t := range traversals {
		if t.RootName() != "local" {
			return ""
		}
	}

	vars := locals()
	if len(vars) == 0 {
		return ""
	}
	val, diags := expr.Value(&hcl.EvalContext{
		Variables: map[string]cty.Value{"local": cty.ObjectVal(vars)},
	})
	if diags.HasErrors() || !val.IsKnown() || val.IsNull() || val.Type() != cty.String {
		return ""
	}
	return val.AsString()
}

func isLocalTerraformModuleSource(raw string) bool {
	for _, prefix := range localModuleSourcePrefixes {
		if strings.HasPrefix(raw, prefix) {
			return true
		}
	}

	return false
}
