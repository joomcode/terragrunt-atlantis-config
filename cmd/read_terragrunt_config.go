package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	_ "unsafe"

	"github.com/gruntwork-io/terragrunt/pkg/config"
	"github.com/gruntwork-io/terragrunt/pkg/log"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// setRenderedOutputs is terragrunt's unexported (*Dependency).setRenderedOutputs. It
// populates a dependency's RenderedOutputs from mock_outputs / applied state (honouring
// SkipOutput), leaving it nil only when nothing is available. We reuse it so mocked and
// disabled dependencies keep real values instead of being forced to unknown.
//
//go:linkname setRenderedOutputs github.com/gruntwork-io/terragrunt/pkg/config.(*Dependency).setRenderedOutputs
func setRenderedOutputs(dep *config.Dependency, ctx context.Context, pctx *config.ParsingContext, l log.Logger) error

// predefinedParseFunctions returns the ParsingContext.PredefinedFunctions map that
// createTerragruntEvalContext copies in last, overriding the built-in HCL functions.
// We override read_terragrunt_config so that nested reads parse with SkipOutput on
// and never cascade on unresolved dependency outputs (see below).
func predefinedParseFunctions(goCtx context.Context, pctx *config.ParsingContext, l log.Logger) map[string]function.Function {
	return map[string]function.Function{
		config.FuncNameReadTerragruntConfig: readTerragruntConfigSkipOutputs(goCtx, pctx, l),
	}
}

// readTerragruntConfigSkipOutputs is a drop-in replacement for terragrunt's
// read_terragrunt_config() HCL function (config.readTerragruntConfigAsFuncImpl).
//
// Terragrunt's version, via config.ParseTerragruntConfig, runs a full parse of the
// target and then setRenderedOutputs. With SkipOutput enabled that leaves each
// Dependency.RenderedOutputs nil, which TerragruntConfigAsCty serialises as a cty
// null under `outputs`; a caller's `read_terragrunt_config(...).dependency.X.outputs.Y`
// then fails with "Attempt to get attribute from null value". We instead substitute
// cty.DynamicVal (unknown), matching what terragrunt itself does on its other
// output-skipping paths (dependency.go dependencyBlocksToCtyValue, config.go ParseConfig).
//
// Stack and values files take a different decode path in terragrunt and do not carry
// dependency outputs, so we delegate those to the stock implementation unchanged.
func readTerragruntConfigSkipOutputs(goCtx context.Context, basePctx *config.ParsingContext, l log.Logger) function.Function {
	return function.New(&function.Spec{
		// One required string param and an optional default value (mirrors terragrunt).
		Params:   []function.Parameter{{Type: cty.String}},
		VarParam: &function.Parameter{Type: cty.DynamicPseudoType},
		Type:     function.StaticReturnType(cty.DynamicPseudoType),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			numParams := len(args)
			if numParams == 0 || numParams > 2 {
				return cty.NilVal, fmt.Errorf("read_terragrunt_config expects 1 or 2 params, got %d", numParams)
			}

			var defaultVal *cty.Value
			if numParams == 2 {
				defaultVal = &args[1]
			}

			target := cleanTargetPath(args[0].AsString(), basePctx.TerragruntConfigPath)

			if !fileExists(target) {
				if defaultVal != nil {
					return *defaultVal, nil
				}
				return cty.NilVal, fmt.Errorf("read_terragrunt_config: target config %s not found", target)
			}

			// Stack / values files: hand back to terragrunt's own handling.
			if base := filepath.Base(target); base == config.DefaultStackFile ||
				base == config.DefaultAutoIncludeStackFile || strings.HasSuffix(base, ".values.hcl") {
				return config.ParseTerragruntConfig(goCtx, basePctx, l, target, defaultVal)
			}

			basePctx.FilesRead.Add(target)

			l2, pctx, err := basePctx.WithConfigPath(l, target)
			if err != nil {
				return cty.NilVal, err
			}
			pctx = pctx.WithDiagnosticsSuppressed(l2)
			pctx.SkipOutput = true
			// The target decodes its own dependency blocks; reset the parent's.
			pctx.DecodedDependencies = nil
			// Rebind the override to the target so nested read_terragrunt_config calls
			// resolve their relative paths against the target, not the caller.
			pctx.PredefinedFunctions = predefinedParseFunctions(goCtx, pctx, l2)

			cfg, err := config.ParseConfigFile(goCtx, pctx, l2, target, nil)
			if err != nil {
				return cty.NilVal, err
			}

			// Populate outputs like stock read_terragrunt_config does (mocks / applied state,
			// no shell-out under SkipOutput), then promote whatever is still unresolved from
			// null to unknown so `dependency.X.outputs.Y` degrades gracefully instead of
			// erroring. TerragruntDependencies is a value slice, so index to mutate in place.
			for i := range cfg.TerragruntDependencies {
				if err := setRenderedOutputs(&cfg.TerragruntDependencies[i], goCtx, pctx, l2); err != nil {
					return cty.NilVal, err
				}
				if cfg.TerragruntDependencies[i].RenderedOutputs == nil {
					dv := cty.DynamicVal
					cfg.TerragruntDependencies[i].RenderedOutputs = &dv
				}
			}

			return config.TerragruntConfigAsCty(cfg)
		},
	})
}

// cleanTargetPath mirrors terragrunt's unexported getCleanedTargetConfigPath: it
// resolves a (possibly relative) read_terragrunt_config target against the directory
// of the calling config, and appends the default config filename for directories.
func cleanTargetPath(configPath, workingPath string) string {
	target := configPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(workingPath), target)
	}
	if fi, err := os.Stat(target); err == nil && fi.IsDir() {
		target = config.GetDefaultConfigPath(target)
	}
	return filepath.Clean(target)
}
