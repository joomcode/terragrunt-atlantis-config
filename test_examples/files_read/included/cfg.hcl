# Included by child4 with a *relative* file() argument, and the file it names exists
# only next to this config — not next to child4. Terragrunt resolves it against the
# directory of the file being parsed, so parsing must succeed. Overriding file() in
# ParsingContext.PredefinedFunctions would resolve it against child4 instead and fail
# generation for the whole repo; this config is the regression guard for that.
locals {
  relative_read = file("data.txt")
}
