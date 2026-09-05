// Package webapi is the thin Go boundary the schema-contract ADR describes:
// generated wire types (package webapiv1, in ./generated) plus handwritten
// conversion from/to the existing service layer. Nothing outside this
// package imports the generated types, and this package never imports
// net/http handler concerns into the service layer.
//
// Regenerate the wire types after editing the TypeSpec source in
// web/api/ — see web/api/README.md for the full command chain.
package webapi

//go:generate go tool oapi-codegen -config oapi-codegen.yaml ../../../web/api/generated/openapi.yaml
