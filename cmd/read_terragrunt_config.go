package cmd

import (
	"bytes"
	"context"
	goerrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// file() / templatefile() are deliberately NOT overridden to record their argument,
	// even though terragrunt does not track them in ParsingContext.FilesRead on its own
	// (unlike read_terragrunt_config / read_tfvars_file / sops_decrypt_file / includes,
	// which it does). A PredefinedFunctions entry cannot tell which config file is being
	// evaluated, and these functions resolve relative paths against exactly that:
	// terragrunt builds the Terraform function scope with BaseDir = filepath.Dir of the
	// file being parsed (createTerragruntEvalContext), while a wrapper closure only has
	// the ParsingContext it was built from. Those differ for every included config:
	// ParseConfigFile takes the include path as a separate argument and never re-points
	// the context, and the TrackInclude that does name the include is set on a clone made
	// by WithIncrementedDepth, so the wrapper cannot observe it either. A wrapper using
	// the context's own dir therefore resolves a relative file() inside an included config
	// against the *child* unit's directory: `file("data.txt")` in a shared included
	// config reads the consuming unit's data.txt, and hard-fails generation for the whole
	// repo when that file does not exist. Recording only absolute arguments is not a fix
	// either, since the override still has to evaluate the relative ones.
	//
	// Files consumed via file()/templatefile() therefore need Terragrunt's native
	// mark_as_read / mark_glob_as_read (tracked with the correct base dir because
	// terragrunt resolves it itself) or an extra_atlantis_dependencies entry. Tracking
	// them automatically belongs upstream in createTerragruntEvalContext, where the
	// config path is in scope.
	//
	// find_in_parent_folders() is not wrapped for a second reason: this map is rebound to
	// the read target inside readTerragruntConfigSkipOutputs so nested
	// read_terragrunt_config paths resolve correctly, which would make an overridden
	// find_in_parent_folders search from the wrong config path and fail. Its results
	// already flow into include / read_terragrunt_config, both of which are tracked.
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

			// Memoize each resolved target's transitive read set + decoded result. The
			// memo is shared across sibling leaves (so each target parses once) yet
			// deterministic: every consumer replays the same stored read set regardless
			// of parse order.
			//
			// Most of a read's caller-dependence (which account.hcl / region.hcl a leaf
			// pulls in) is already baked into the resolved target path. The one
			// exception is a shared config that itself calls get_original_terragrunt_dir(),
			// whose reads resolve relative to the *consuming stack's* dir. Such targets are keyed
			// additionally by that stack dir; targets that don't consume it stay shared.
			mv, err := lookupOrParseReadTarget(goCtx, basePctx, l, target)
			if err != nil {
				return cty.NilVal, err
			}
			for _, pth := range mv.reads {
				basePctx.FilesRead.Add(pth)
			}
			return mv.result, nil
		},
	})
}

// readMemoValue is a memoized read_terragrunt_config target: its transitive read
// set (absolute, cleaned, including the target itself) and its decoded cty result.
type readMemoValue struct {
	reads  []string
	result cty.Value
}

// readMemoEntry holds the memoized captures for one target path. A target whose
// parse never consumes get_original_terragrunt_dir is caller-independent and stored
// once in `shared`. A target that does consume it is caller-dependent and stored
// per consuming stack dir in `byOrigDir` (rare in practice, so the extra parses are
// negligible).
type readMemoEntry struct {
	mu               sync.Mutex
	consumedOriginal bool
	shared           *readMemoValue
	byOrigDir        map[string]readMemoValue
}

// get returns the memoized value for origDir, or ok=false if nothing is stored yet.
func (e *readMemoEntry) get(origDir string) (mv readMemoValue, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.consumedOriginal {
		v, found := e.byOrigDir[origDir]
		return v, found
	}
	if e.shared != nil {
		return *e.shared, true
	}
	return readMemoValue{}, false
}

// put stores a capture, promoting the entry to caller-dependent the first time a
// capture is observed to consume get_original_terragrunt_dir.
func (e *readMemoEntry) put(origDir string, mv readMemoValue, consumed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if consumed && !e.consumedOriginal {
		e.consumedOriginal = true
		e.shared = nil
		e.byOrigDir = map[string]readMemoValue{}
	}
	if e.consumedOriginal {
		e.byOrigDir[origDir] = mv
		return
	}
	v := mv
	e.shared = &v
}

// readMemo maps a resolved target path to its *readMemoEntry.
var readMemo sync.Map // string -> *readMemoEntry

func memoEntry(target string) *readMemoEntry {
	if v, ok := readMemo.Load(target); ok {
		return v.(*readMemoEntry)
	}
	e, _ := readMemo.LoadOrStore(target, &readMemoEntry{})
	return e.(*readMemoEntry)
}

// lookupOrParseReadTarget returns the memoized read set + result for target,
// parsing it once per (target, consuming stack dir) and replaying the stored
// value thereafter. Concurrent first parses of the same key may duplicate work,
// but they compute identical values, so the last idempotent put wins.
func lookupOrParseReadTarget(goCtx context.Context, basePctx *config.ParsingContext, l log.Logger, target string) (readMemoValue, error) {
	origDir := filepath.Dir(basePctx.OriginalTerragruntConfigPath)
	entry := memoEntry(target)
	if mv, ok := entry.get(origDir); ok {
		return mv, nil
	}
	mv, consumed, err := captureReadTarget(goCtx, basePctx, l, target)
	if err != nil {
		return readMemoValue{}, err
	}
	entry.put(origDir, mv, consumed)
	return mv, nil
}

// originalDirToken is the function name whose presence makes a read target's read
// set caller-dependent. HCL2 function call names are static identifiers (and in
// .hcl.json configs calls sit inside "${...}" strings), so an evaluated call always
// leaves this literal in the file bytes. False positives (the name in a comment or
// string) merely key the target per consuming dir — extra parses, same output.
const originalDirToken = "get_original_terragrunt_dir"

// readsConsumeOriginalDir reports whether any file in a capture's transitive read
// set mentions get_original_terragrunt_dir. Every file whose HCL is evaluated with
// terragrunt functions during a capture is in the set (the target itself, includes,
// nested read targets and their reads, tfvars), so scanning it also covers
// consumption nested arbitrarily deep. Unreadable files are skipped: their content
// was never evaluated here, so they cannot have consumed the function.
func readsConsumeOriginalDir(reads []string) bool {
	for _, p := range reads {
		b, err := os.ReadFile(p)
		if err == nil && bytes.Contains(b, []byte(originalDirToken)) {
			return true
		}
	}
	return false
}

// captureReadTarget parses target into an isolated FilesRead. Returns the target's
// transitive read set (plus the target), its decoded cty value, and whether the set
// consumes get_original_terragrunt_dir (making it caller-dependent).
func captureReadTarget(goCtx context.Context, basePctx *config.ParsingContext, l log.Logger, target string) (readMemoValue, bool, error) {
	l2, pctx, err := deriveTargetPctx(goCtx, basePctx, l, target)
	if err != nil {
		return readMemoValue{}, false, err
	}
	// Isolate this target's reads so we memoize just its transitive set. The
	// PredefinedFunctions wrappers and nested override read pctx.FilesRead lazily,
	// so they land in this fresh set.
	pctx.FilesRead = config.NewFilesRead()
	result, err := parseTargetConfig(goCtx, pctx, l2, target)
	if err != nil {
		return readMemoValue{}, false, err
	}
	reads := append(pctx.FilesRead.Paths(), target)
	// Caller-dependence is a property of the files that were actually evaluated, so
	// decide it before folding in the declared dependencies below: one of those may
	// well be a readable file mentioning get_original_terragrunt_dir without the
	// target's own parse ever having consumed it.
	consumed := readsConsumeOriginalDir(reads)

	deps, err := declaredExtraDeps(result, target, l2)
	if err != nil {
		return readMemoValue{}, false, err
	}
	reads = append(reads, deps...)

	return readMemoValue{reads: reads, result: result}, consumed, nil
}

// declaredExtraDeps returns the extra_atlantis_dependencies a read target declares,
// resolved against the target's own directory. Nothing else can surface these: they
// are paths terragrunt never reads (files a terraform module consumes at apply,
// globs, runtime-built paths), which is exactly why they are declared by hand. Adding
// them to the target's memoized read set is what propagates them to every consumer —
// transitively, since a nested read target's declarations are already inside the
// outer target's set, and independent of parse order.
//
// The target's directory is the right base: read_terragrunt_config carries no include
// tracking (path_relative_to_include() is "." there), so a shared config has no way to
// name a path relative to the consuming unit, and terragrunt's own mark_as_read anchors
// relative paths the same way. An include parent is the opposite case and keeps its
// existing child-relative semantics — see mergeResolvedLocals.
func declaredExtraDeps(result cty.Value, target string, l log.Logger) ([]string, error) {
	if result == cty.NilVal || result.IsNull() || !result.Type().IsObjectType() ||
		!result.Type().HasAttribute("locals") {
		return nil, nil
	}

	deps, err := extraDepsFromLocals(result.GetAttr("locals"))
	if goerrors.Is(err, errLocalNeedsOutputs) {
		// An entry steered by a dependency output cannot be resolved while outputs are
		// skipped. Skipping it loses a dependency; re-parsing the target with outputs
		// resolved would shell out per target. Warn and move on — the unit can always
		// declare the path itself.
		l.Warnf("skipping extra_atlantis_dependencies of %s: unresolved dependency outputs", target)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}

	dir := filepath.Dir(target)
	resolved := make([]string, 0, len(deps))
	for _, dep := range deps {
		if dep == "" || filepath.IsAbs(dep) {
			resolved = append(resolved, dep)
			continue
		}
		// Globs are kept as globs: a new file matching one must not need a regenerated
		// config, so nothing is expanded here.
		resolved = append(resolved, filepath.ToSlash(filepath.Join(dir, dep)))
	}

	return resolved, nil
}

// deriveTargetPctx builds the parsing context for a read_terragrunt_config target:
// switch to the target path, suppress diagnostics, skip outputs, reset the parent's
// decoded dependencies, and rebind the override so nested reads resolve against the
// target rather than the caller.
func deriveTargetPctx(goCtx context.Context, basePctx *config.ParsingContext, l log.Logger, target string) (log.Logger, *config.ParsingContext, error) {
	l2, pctx, err := basePctx.WithConfigPath(l, target)
	if err != nil {
		return nil, nil, err
	}
	pctx = pctx.WithDiagnosticsSuppressed(l2)
	pctx.SkipOutput = true
	pctx.DecodedDependencies = nil
	pctx.PredefinedFunctions = predefinedParseFunctions(goCtx, pctx, l2)
	return l2, pctx, nil
}

// parseTargetConfig parses the target and promotes unresolved dependency outputs
// from null to cty.DynamicVal, mirroring stock read_terragrunt_config under SkipOutput.
func parseTargetConfig(goCtx context.Context, pctx *config.ParsingContext, l log.Logger, target string) (cty.Value, error) {
	cfg, err := config.ParseConfigFile(goCtx, pctx, l, target, nil)
	if err != nil {
		return cty.NilVal, err
	}
	for i := range cfg.TerragruntDependencies {
		if err := setRenderedOutputs(&cfg.TerragruntDependencies[i], goCtx, pctx, l); err != nil {
			return cty.NilVal, err
		}
		if cfg.TerragruntDependencies[i].RenderedOutputs == nil {
			dv := cty.DynamicVal
			cfg.TerragruntDependencies[i].RenderedOutputs = &dv
		}
	}
	return config.TerragruntConfigAsCty(cfg)
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
