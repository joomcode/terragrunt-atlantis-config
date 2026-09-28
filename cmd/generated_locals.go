package cmd

import (
	"context"
	"strings"
	"sync"

	"github.com/gruntwork-io/terragrunt/pkg/config"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

var localsBlockSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: "locals"}},
}

// generatedLocalsFor returns the literal locals that the generate blocks of the
// terragrunt config at path write into the Terraform working directory.
//
// Terragrunt's partial parse does not decode generate blocks, so this runs a full
// parse, at most once per config and only when a module source needs it.
func generatedLocalsFor(goCtx context.Context, basePctx *config.ParsingContext, path string) rootLocals {
	return sync.OnceValue(func() map[string]cty.Value {
		l, pctx, err := deriveTargetPctx(goCtx, basePctx, tgLogger, path)
		if err == nil {
			// Files read by the full parse must not leak into the project's when_modified.
			pctx.FilesRead = config.NewFilesRead()
			var cfg *config.TerragruntConfig
			cfg, err = config.ParseConfigFile(goCtx, pctx, l, path, nil)
			if err == nil {
				return literalLocals(cfg)
			}
		}
		tgLogger.Warnf("Cannot resolve locals in module sources of %s: %v", path, err)
		return nil
	})
}

func literalLocals(cfg *config.TerragruntConfig) map[string]cty.Value {
	locals := map[string]cty.Value{}
	for _, gen := range cfg.GenerateConfigs {
		if gen.Disable || !strings.HasSuffix(gen.Path, ".tf") {
			continue
		}
		file, diags := hclsyntax.ParseConfig([]byte(gen.Contents), gen.Path, hcl.InitialPos)
		if diags.HasErrors() {
			continue
		}
		content, _, _ := file.Body.PartialContent(localsBlockSchema)
		for _, block := range content.Blocks {
			attrs, _ := block.Body.JustAttributes()
			for name, attr := range attrs {
				if len(attr.Expr.Variables()) > 0 {
					continue
				}
				if val, diags := attr.Expr.Value(nil); !diags.HasErrors() {
					locals[name] = val
				}
			}
		}
	}
	return locals
}
