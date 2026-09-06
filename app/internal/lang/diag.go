// Package lang loads and validates the Plecture configuration
// language: reserved root files, plugin/catalog manifests, definition
// discovery across trust layers, and reference resolution. It is new,
// parallel infrastructure — the existing per-surface loaders in
// app/internal/config and app/internal/plugins keep governing runtime
// behavior until a later slice cuts surfaces over.
package lang

import "fmt"

// Layer names one of the language's four validation layers, in the order a
// load pipeline applies them: structural, semantic, CEL, and instantiation
// (the last is checked only once an instance actually binds).
type Layer string

const (
	LayerStructural    Layer = "structural"
	LayerSemantic      Layer = "semantic"
	LayerCEL           Layer = "cel"
	LayerInstantiation Layer = "instantiation"
)

// Code is one of the language's PLECTURE-CFG-* diagnostic codes, documented
// in docs/language/README.md.
type Code string

const (
	CodeKindMissing                   Code = "PLECTURE-CFG-KIND-MISSING"
	CodeKindUnknown                   Code = "PLECTURE-CFG-KIND-UNKNOWN"
	CodeIDInvalid                     Code = "PLECTURE-CFG-ID-INVALID"
	CodeFieldUnknown                  Code = "PLECTURE-CFG-FIELD-UNKNOWN"
	CodeFieldRequired                 Code = "PLECTURE-CFG-FIELD-REQUIRED"
	CodeFieldType                     Code = "PLECTURE-CFG-FIELD-TYPE"
	CodeSchemaVersionOlder            Code = "PLECTURE-CFG-SCHEMA-VERSION-OLDER"
	CodeSchemaVersionNewer            Code = "PLECTURE-CFG-SCHEMA-VERSION-NEWER"
	CodeValueFromAndExpr              Code = "PLECTURE-CFG-VALUE-FROM-AND-EXPR"
	CodeValueDefaultAndOptional       Code = "PLECTURE-CFG-VALUE-DEFAULT-AND-OPTIONAL"
	CodeValueTagUnknown               Code = "PLECTURE-CFG-VALUE-TAG-UNKNOWN"
	CodeValueTagSurface               Code = "PLECTURE-CFG-VALUE-TAG-SURFACE"
	CodeActionTypeUnknown             Code = "PLECTURE-CFG-ACTION-TYPE-UNKNOWN"
	CodeActionVariant                 Code = "PLECTURE-CFG-ACTION-VARIANT"
	CodeActionBinAndCommand           Code = "PLECTURE-CFG-ACTION-BIN-AND-COMMAND"
	CodeShellInterpolation            Code = "PLECTURE-CFG-SHELL-INTERPOLATION"
	CodeRefDynamic                    Code = "PLECTURE-CFG-REF-DYNAMIC"
	CodeChannelTimeoutRoot            Code = "PLECTURE-CFG-CHANNEL-TIMEOUT-ROOT"
	CodeTaskInstructionElement        Code = "PLECTURE-CFG-TASK-INSTRUCTION-ELEMENT"
	CodeUnknownRef                    Code = "PLECTURE-CFG-UNKNOWN-REF"
	CodeKindMismatch                  Code = "PLECTURE-CFG-KIND-MISMATCH"
	CodeIDDuplicate                   Code = "PLECTURE-CFG-ID-DUPLICATE"
	CodeRefAliasRequired              Code = "PLECTURE-CFG-REF-ALIAS-REQUIRED"
	CodeRefCrossPlugin                Code = "PLECTURE-CFG-REF-CROSS-PLUGIN"
	CodeTaskInstructionFileMissing    Code = "PLECTURE-CFG-TASK-INSTRUCTION-FILE-MISSING"
	CodeTaskInstructionFileCrossLayer Code = "PLECTURE-CFG-TASK-INSTRUCTION-FILE-CROSS-LAYER"
	CodeTaskInstructionControlFlow    Code = "PLECTURE-CFG-TASK-INSTRUCTION-CONTROL-FLOW"
	CodeFromRoot                      Code = "PLECTURE-CFG-FROM-ROOT"
	CodeFromPath                      Code = "PLECTURE-CFG-FROM-PATH"
	CodeResourceObserverMismatch      Code = "PLECTURE-CFG-RESOURCE-OBSERVER-MISMATCH"
	CodeFirstObserveFailed            Code = "PLECTURE-CFG-FIRST-OBSERVE-FAILED"
	CodeBinUnknown                    Code = "PLECTURE-CFG-BIN-UNKNOWN"
	CodeTerminalUnavailable           Code = "PLECTURE-CFG-TERMINAL-UNAVAILABLE"
	CodeHealthAliveRequired           Code = "PLECTURE-CFG-HEALTH-ALIVE-REQUIRED"
	CodeNestingCycle                  Code = "PLECTURE-CFG-NESTING-CYCLE"
	CodeNestingOutputMutable          Code = "PLECTURE-CFG-NESTING-OUTPUT-MUTABLE"
	CodeNestingProjectionMismatch     Code = "PLECTURE-CFG-NESTING-PROJECTION-MISMATCH"
	CodeExtendsInheritedField         Code = "PLECTURE-CFG-EXTENDS-INHERITED-FIELD"
	CodeExtendsCycle                  Code = "PLECTURE-CFG-EXTENDS-CYCLE"
	CodeExtendsJudgeIDDuplicate       Code = "PLECTURE-CFG-EXTENDS-JUDGE-ID-DUPLICATE"
	CodeExtendsChainIDDuplicate       Code = "PLECTURE-CFG-EXTENDS-CHAIN-ID-DUPLICATE"
	CodeExtendsDefaultRedeclared      Code = "PLECTURE-CFG-EXTENDS-DEFAULT-REDECLARED"
	CodeExtendsSchemaType             Code = "PLECTURE-CFG-EXTENDS-SCHEMA-TYPE"
	CodeExtendsSchemaShape            Code = "PLECTURE-CFG-EXTENDS-SCHEMA-SHAPE"
	CodeExtendsSchemaFileUnsupported  Code = "PLECTURE-CFG-EXTENDS-SCHEMA-FILE-UNSUPPORTED"
	CodeWorkflowCycle                 Code = "PLECTURE-CFG-WORKFLOW-CYCLE"
	CodePopulationContract            Code = "PLECTURE-CFG-POPULATION-CONTRACT"
	CodeCELSyntax                     Code = "PLECTURE-CFG-CEL-SYNTAX"
	CodeCELUnknownName                Code = "PLECTURE-CFG-CEL-UNKNOWN-NAME"
	CodeCELType                       Code = "PLECTURE-CFG-CEL-TYPE"
	CodeCELCustomFunction             Code = "PLECTURE-CFG-CEL-CUSTOM-FUNCTION"
)

// codeLayers documents which layer(s) each code is legitimately reported at,
// mirroring the table in docs/language/README.md. Every code maps to exactly
// one layer except PLECTURE-CFG-FROM-ROOT, which is structural on a surface
// whose roots are a fixed prefix set and semantic otherwise — a Diagnostic
// still names its layer explicitly at each construction site rather than
// deriving it from this table.
var codeLayers = map[Code][]Layer{
	CodeKindMissing:                   {LayerStructural},
	CodeKindUnknown:                   {LayerStructural},
	CodeIDInvalid:                     {LayerStructural},
	CodeFieldUnknown:                  {LayerStructural},
	CodeFieldRequired:                 {LayerStructural},
	CodeFieldType:                     {LayerStructural},
	CodeSchemaVersionOlder:            {LayerSemantic},
	CodeSchemaVersionNewer:            {LayerSemantic},
	CodeValueFromAndExpr:              {LayerStructural},
	CodeValueDefaultAndOptional:       {LayerStructural},
	CodeValueTagUnknown:               {LayerStructural},
	CodeValueTagSurface:               {LayerStructural},
	CodeActionTypeUnknown:             {LayerStructural},
	CodeActionVariant:                 {LayerStructural},
	CodeActionBinAndCommand:           {LayerStructural},
	CodeShellInterpolation:            {LayerStructural},
	CodeRefDynamic:                    {LayerStructural},
	CodeChannelTimeoutRoot:            {LayerStructural},
	CodeTaskInstructionElement:        {LayerStructural},
	CodeUnknownRef:                    {LayerSemantic},
	CodeKindMismatch:                  {LayerSemantic},
	CodeIDDuplicate:                   {LayerSemantic},
	CodeRefAliasRequired:              {LayerSemantic},
	CodeRefCrossPlugin:                {LayerSemantic},
	CodeTaskInstructionFileMissing:    {LayerSemantic},
	CodeTaskInstructionFileCrossLayer: {LayerSemantic},
	CodeTaskInstructionControlFlow:    {LayerSemantic},
	CodeFromRoot:                      {LayerStructural, LayerSemantic},
	CodeFromPath:                      {LayerSemantic},
	CodeResourceObserverMismatch:      {LayerInstantiation},
	CodeFirstObserveFailed:            {LayerInstantiation},
	CodeBinUnknown:                    {LayerSemantic},
	CodeTerminalUnavailable:           {LayerSemantic},
	CodeHealthAliveRequired:           {LayerStructural},
	CodeNestingCycle:                  {LayerSemantic},
	CodeNestingOutputMutable:          {LayerSemantic},
	CodeNestingProjectionMismatch:     {LayerSemantic},
	CodeExtendsInheritedField:         {LayerStructural},
	CodeExtendsCycle:                  {LayerSemantic},
	CodeExtendsJudgeIDDuplicate:       {LayerSemantic},
	CodeExtendsChainIDDuplicate:       {LayerSemantic},
	CodeExtendsDefaultRedeclared:      {LayerSemantic},
	CodeExtendsSchemaType:             {LayerSemantic},
	CodeExtendsSchemaShape:            {LayerStructural},
	CodeExtendsSchemaFileUnsupported:  {LayerSemantic},
	CodeWorkflowCycle:                 {LayerSemantic},
	CodePopulationContract:            {LayerSemantic},
	CodeCELSyntax:                     {LayerCEL},
	CodeCELUnknownName:                {LayerCEL},
	CodeCELType:                       {LayerCEL},
	CodeCELCustomFunction:             {LayerCEL},
}

// Codes returns every documented diagnostic code, in the order declared
// above.
func Codes() []Code {
	// The literal order above already matches docs/language/README.md's
	// table, so codeLayers (a map, unordered) is not the iteration source.
	return []Code{
		CodeKindMissing, CodeKindUnknown, CodeIDInvalid, CodeFieldUnknown,
		CodeFieldRequired, CodeFieldType, CodeSchemaVersionOlder, CodeSchemaVersionNewer,
		CodeValueFromAndExpr, CodeValueDefaultAndOptional, CodeValueTagUnknown, CodeValueTagSurface,
		CodeActionTypeUnknown, CodeActionVariant, CodeActionBinAndCommand, CodeShellInterpolation,
		CodeRefDynamic, CodeChannelTimeoutRoot, CodeTaskInstructionElement, CodeUnknownRef,
		CodeKindMismatch, CodeIDDuplicate,
		CodeRefAliasRequired, CodeRefCrossPlugin,
		CodeTaskInstructionFileMissing, CodeTaskInstructionFileCrossLayer, CodeTaskInstructionControlFlow,
		CodeFromRoot, CodeFromPath,
		CodeResourceObserverMismatch, CodeFirstObserveFailed, CodeBinUnknown, CodeTerminalUnavailable,
		CodeHealthAliveRequired,
		CodeNestingCycle, CodeNestingOutputMutable, CodeNestingProjectionMismatch,
		CodeExtendsInheritedField, CodeExtendsCycle, CodeExtendsJudgeIDDuplicate,
		CodeExtendsChainIDDuplicate, CodeExtendsDefaultRedeclared, CodeExtendsSchemaType,
		CodeExtendsSchemaShape, CodeExtendsSchemaFileUnsupported, CodeWorkflowCycle, CodePopulationContract,
		CodeCELSyntax, CodeCELUnknownName, CodeCELType, CodeCELCustomFunction,
	}
}

// ValidLayer reports whether layer is one docs/language/README.md documents
// for code.
func ValidLayer(code Code, layer Layer) bool {
	for _, l := range codeLayers[code] {
		if l == layer {
			return true
		}
	}
	return false
}

// Position locates a diagnostic within a definition tree: the file it was
// read from, and — since BurntSushi/toml's decode API exposes key paths but
// not line/column positions — the dotted key path within that file. Path is
// empty for a file-level diagnostic (e.g. a missing top-level field).
type Position struct {
	File string
	Path string
}

func (p Position) String() string {
	if p.Path == "" {
		return p.File
	}
	return fmt.Sprintf("%s: %s", p.File, p.Path)
}

// Diagnostic is one violation of a language rule. It implements error so a
// loader can return it (or wrap it) directly; callers that need the code or
// layer use errors.As.
type Diagnostic struct {
	Code   Code
	Layer  Layer
	Reason string
	Pos    Position
}

func (d *Diagnostic) Error() string {
	if d.Pos.File == "" {
		return fmt.Sprintf("%s: %s", d.Code, d.Reason)
	}
	return fmt.Sprintf("%s: %s: %s", d.Pos, d.Code, d.Reason)
}

// newDiag constructs a Diagnostic, panicking if layer is not one
// docs/language/README.md documents for code — a programmer error (a typo
// at a construction site), never a data-dependent condition.
func newDiag(code Code, layer Layer, pos Position, reason string) *Diagnostic {
	if !ValidLayer(code, layer) {
		panic(fmt.Sprintf("lang: %s is not a documented layer for %s", layer, code))
	}
	return &Diagnostic{Code: code, Layer: layer, Reason: reason, Pos: pos}
}
