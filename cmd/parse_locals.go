package cmd

// Terragrunt doesn't give us an easy way to access all of the Locals from a module
// in an easy to digest way. This file is mostly just follows along how Terragrunt
// parses the `locals` blocks and evaluates their contents.

import (
	"context"
	goerrors "errors"
	"fmt"
	"github.com/gruntwork-io/go-commons/errors"
	"github.com/gruntwork-io/terragrunt/pkg/config"
	"github.com/gruntwork-io/terragrunt/pkg/config/hclparse"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"path/filepath"
	"sync"

	"golang.org/x/sync/singleflight"
)

// ResolvedLocals are the parsed result of local values this module cares about
type ResolvedLocals struct {
	// The Atlantis workflow to use for some project
	AtlantisWorkflow string

	// Apply requirements to override the global `--apply-requirements` flag
	ApplyRequirements []string

	// Extra dependencies that can be hardcoded in config
	ExtraAtlantisDependencies []string

	// If set, a single module will have autoplan turned to this setting
	AutoPlan *bool

	// If set to true, the module will not be included in the output
	Skip *bool

	// Terraform version to use just for this project
	TerraformVersion string

	// If set to true, create Atlantis project
	markedProject *bool
}

// parseHcl uses the HCL2 parser to parse the given string into an HCL file body.
func parseHcl(parser *hclparse.Parser, hcl string, filename string) (file *hcl.File, err error) {
	// The HCL2 parser and especially cty conversions will panic in many types of errors, so we have to recover from
	// those panics here and convert them to normal errors
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.WithStackTrace(hclparse.PanicWhileParsingConfigError{RecoveredValue: recovered, ConfigFile: filename})
		}
	}()

	if filepath.Ext(filename) == ".json" {
		file, parseDiagnostics := parser.ParseJSON([]byte(hcl), filename)
		if parseDiagnostics != nil && parseDiagnostics.HasErrors() {
			return nil, parseDiagnostics
		}

		return file, nil
	}

	file, parseDiagnostics := parser.ParseHCL([]byte(hcl), filename)
	if parseDiagnostics != nil && parseDiagnostics.HasErrors() {
		return nil, parseDiagnostics
	}

	return file, nil
}

// Merges in values from a child into a parent set of `local` values
func mergeResolvedLocals(parent ResolvedLocals, child ResolvedLocals) ResolvedLocals {
	if child.AtlantisWorkflow != "" {
		parent.AtlantisWorkflow = child.AtlantisWorkflow
	}

	if child.TerraformVersion != "" {
		parent.TerraformVersion = child.TerraformVersion
	}

	if child.AutoPlan != nil {
		parent.AutoPlan = child.AutoPlan
	}

	if child.Skip != nil {
		parent.Skip = child.Skip
	}

	if child.markedProject != nil {
		parent.markedProject = child.markedProject
	}

	if child.ApplyRequirements != nil || len(child.ApplyRequirements) > 0 {
		parent.ApplyRequirements = child.ApplyRequirements
	}

	// Entries are appended verbatim, so a relative one declared in the parent ends up
	// resolved against the *child* (getDependencies -> makePathAbsolute). Do not
	// "fix" that into symmetry with the read_terragrunt_config path: a parent names
	// child-local files through path_relative_to_include(), e.g. the conditional
	// local_tags.yaml in test_examples/parent_with_extra_deps/parent/terragrunt.hcl,
	// and resolving it against the parent would point at a file that does not exist.
	// A read target has no include tracking (path_relative_to_include() is "." there),
	// so its own dir is the only base it can name — see declaredExtraDeps in
	// cmd/read_terragrunt_config.go.
	parent.ExtraAtlantisDependencies = append(parent.ExtraAtlantisDependencies, child.ExtraAtlantisDependencies...)

	return parent
}

// Caches the result of parseLocals for top-level (includeFromChild == nil) calls.
// createProject calls parseLocals once directly and once indirectly via getDependencies
// for the same path with includeFromChild == nil; the result is deterministic for a
// given path, so we memoize it to avoid parsing the file (and recursing into its
// parents) twice. Parent recursion (includeFromChild != nil) is NOT cached because
// parent locals can depend on the child path (e.g. find_in_parent_folders,
// path_relative_to_include), so they differ per child.
type resolvedLocalsOutput struct {
	locals ResolvedLocals
	err    error
}

var parseLocalsCache sync.Map // path string -> resolvedLocalsOutput
var parseLocalsGroup singleflight.Group

// Parses a given file, returning a map of all it's `local` values
func parseLocals(goCtx context.Context, ctx *config.ParsingContext, path string, includeFromChild *config.IncludeConfig) (ResolvedLocals, error) {
	if includeFromChild != nil {
		return parseLocalsUncached(goCtx, ctx, path, includeFromChild)
	}

	if v, ok := parseLocalsCache.Load(path); ok {
		out := v.(resolvedLocalsOutput)
		return out.locals, out.err
	}

	res, _, _ := parseLocalsGroup.Do(path, func() (interface{}, error) {
		locals, err := parseLocalsUncached(goCtx, ctx, path, nil)
		out := resolvedLocalsOutput{locals: locals, err: err}
		parseLocalsCache.Store(path, out)
		return out, nil
	})
	out := res.(resolvedLocalsOutput)
	return out.locals, out.err
}

func parseLocalsUncached(goCtx context.Context, ctx *config.ParsingContext, path string, includeFromChild *config.IncludeConfig) (ResolvedLocals, error) {
	file, err := hclparse.NewParser(ctx.ParserOptions...).ParseFromFile(path)
	if err != nil {
		return ResolvedLocals{}, err
	}

	// Decode just the Base blocks. See the function docs for DecodeBaseBlocks for more info on what base blocks are.
	baseBlocks, err := config.DecodeBaseBlocks(goCtx, ctx, tgLogger, file, includeFromChild)
	if err != nil {
		return ResolvedLocals{}, err
	}

	// Recurse on the parent to merge in the locals from that file
	mergedParentLocals := ResolvedLocals{}
	if baseBlocks.TrackInclude != nil && includeFromChild == nil {
		for _, includeConfig := range baseBlocks.TrackInclude.CurrentList {
			parentLocals, _ := parseLocals(goCtx, ctx, includeConfig.Path, &includeConfig)
			mergedParentLocals = mergeResolvedLocals(mergedParentLocals, parentLocals)
		}
	}
	childLocals, err := resolveLocals(*baseBlocks.Locals)
	// A local tac cares about resolved to an unknown value because outputs were skipped.
	// Treat it as resolvable: re-decode just this file's locals with outputs enabled and
	// use that. Parents are untouched (they self-heal through their own parseLocals).
	if goerrors.Is(err, errLocalNeedsOutputs) {
		fullBlocks, ferr := config.DecodeBaseBlocks(goCtx, fullOutputsContext(ctx), tgLogger, file, includeFromChild)
		if ferr != nil {
			return ResolvedLocals{}, ferr
		}
		childLocals, err = resolveLocals(*fullBlocks.Locals)
	}
	if err != nil {
		return ResolvedLocals{}, err
	}
	return mergeResolvedLocals(mergedParentLocals, childLocals), nil
}

// errLocalNeedsOutputs signals that a consumed local resolved to an unknown value
// (because we parse with SkipOutput), so the file must be re-parsed with outputs.
var errLocalNeedsOutputs = goerrors.New("atlantis local requires resolved outputs")

// fullOutputsContext returns a clone of ctx that resolves dependency outputs: SkipOutput
// off, terragrunt's stock read_terragrunt_config (our override cleared), and the partial
// parse cache disabled so a previously-cached skipped-output result is not reused.
func fullOutputsContext(ctx *config.ParsingContext) *config.ParsingContext {
	c := ctx.Clone()
	c.SkipOutput = false
	c.PredefinedFunctions = nil
	c.UsePartialParseConfigCache = false
	return c
}

func resolveLocals(localsAsCty cty.Value) (ResolvedLocals, error) {
	resolved := ResolvedLocals{}

	// Return an empty set of locals if no `locals` block was present
	if localsAsCty == cty.NilVal {
		return resolved, nil
	}
	rawLocals := localsAsCty.AsValueMap()

	workflowValue, ok := rawLocals["atlantis_workflow"]
	if ok {
		if !workflowValue.IsKnown() {
			return resolved, errLocalNeedsOutputs
		}
		resolved.AtlantisWorkflow = workflowValue.AsString()
	}

	versionValue, ok := rawLocals["atlantis_terraform_version"]
	if ok {
		if !versionValue.IsKnown() {
			return resolved, errLocalNeedsOutputs
		}
		resolved.TerraformVersion = versionValue.AsString()
	}

	autoPlanValue, ok := rawLocals["atlantis_autoplan"]
	if ok {
		if !autoPlanValue.IsKnown() {
			return resolved, errLocalNeedsOutputs
		}
		hasValue := autoPlanValue.True()
		resolved.AutoPlan = &hasValue
	}

	skipValue, ok := rawLocals["atlantis_skip"]
	if ok {
		if !skipValue.IsKnown() {
			return resolved, errLocalNeedsOutputs
		}
		hasValue := skipValue.True()
		resolved.Skip = &hasValue
	}

	applyReqs, ok := rawLocals["atlantis_apply_requirements"]
	if ok {
		if !applyReqs.IsKnown() {
			return resolved, errLocalNeedsOutputs
		}
		resolved.ApplyRequirements = []string{}
		it := applyReqs.ElementIterator()
		for it.Next() {
			_, val := it.Element()
			if !val.IsKnown() {
				return resolved, errLocalNeedsOutputs
			}
			resolved.ApplyRequirements = append(resolved.ApplyRequirements, val.AsString())
		}
	}

	markedProject, ok := rawLocals["atlantis_project"]
	if ok {
		if !markedProject.IsKnown() {
			return resolved, errLocalNeedsOutputs
		}
		hasValue := markedProject.True()
		resolved.markedProject = &hasValue
	}

	extraDependencies, err := extraDepsFromLocals(localsAsCty)
	if err != nil {
		return resolved, err
	}
	resolved.ExtraAtlantisDependencies = extraDependencies

	return resolved, nil
}

// extraDepsFromLocals pulls extra_atlantis_dependencies out of an already decoded
// `locals` map, returning the entries verbatim (globs included). Shared with the
// read_terragrunt_config override, which propagates a read target's declared
// dependencies to its consumers.
func extraDepsFromLocals(localsAsCty cty.Value) ([]string, error) {
	if localsAsCty == cty.NilVal || localsAsCty.IsNull() || !localsAsCty.IsKnown() {
		return nil, nil
	}
	if !localsAsCty.Type().IsObjectType() && !localsAsCty.Type().IsMapType() {
		return nil, nil
	}

	extraDependenciesAsCty, ok := localsAsCty.AsValueMap()["extra_atlantis_dependencies"]
	if !ok || extraDependenciesAsCty.IsNull() {
		return nil, nil
	}
	if !extraDependenciesAsCty.IsKnown() {
		return nil, errLocalNeedsOutputs
	}

	var deps []string
	it := extraDependenciesAsCty.ElementIterator()
	for it.Next() {
		pos, val := it.Element()
		if !val.IsKnown() {
			return nil, errLocalNeedsOutputs
		}
		if !val.Type().Equals(cty.String) {
			posInt, _ := pos.AsBigFloat().Int64()
			return nil, fmt.Errorf("extra_atlantis_dependencies contains non-string value at position %d", posInt)
		}

		deps = append(deps, filepath.ToSlash(val.AsString()))
	}

	return deps, nil
}
