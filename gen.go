package korai

// Code generation for the typed transport core.
//
// oapi-codegen does not yet support OpenAPI 3.1 (the spec uses 3.1's
// `type: [T, "null"]` nullable syntax), so we down-convert the PUBLIC spec
// to 3.0 first, then generate. The input is specs/openapi.public.yaml — the
// derived public surface (canonical spec minus every `x-internal: true`
// operation), built by openapi-format ahead of codegen (see scripts/codegen.sh
// and specs/openapi-format.filter.yaml). The intermediate specs.v30.yaml
// is gitignored. Output lands in koraiapi/, which IS committed: the
// hand-written ergonomic layer in this package imports it and Go has no
// build-time codegen hook, so the module must build from a clean
// checkout. Re-run after changing the spec and commit koraiapi/.
//
// Run with: scripts/codegen.sh go (derives the public spec first), or
// `go generate ./...` if specs/openapi.public.yaml is already built.
// Requires `bunx` and network access.

//go:generate bunx @apiture/openapi-down-convert@latest --input ../../specs/openapi.public.yaml --output specs.v30.yaml
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest -config oapi-codegen.yaml specs.v30.yaml
