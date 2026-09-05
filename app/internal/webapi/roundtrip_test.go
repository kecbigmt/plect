package webapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

// The template-instantiated list envelope round-trips: decode, re-encode,
// decode again, same shape — proving oapi-codegen's generated
// SessionListResponse (from TypeSpec's ListResponse<SessionSummary>) is a
// faithful Go representation of the emitted OpenAPI schema, not just that it
// compiles.
func TestRoundTrip_SessionListEnvelope(t *testing.T) {
	raw := readTestdata(t, "session_list.valid.json")

	var decoded webapiv1.SessionListResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Count != 2 || len(decoded.Items) != 2 {
		t.Fatalf("decoded = %+v, want 2 items", decoded)
	}
	if decoded.Items[1].ResourceId != "" {
		t.Errorf("Items[1].ResourceId = %q, want empty (required field, empty value)", decoded.Items[1].ResourceId)
	}
	if decoded.Items[1].Branch != nil {
		t.Errorf("Items[1].Branch = %v, want nil (omitted, not present in fixture)", decoded.Items[1].Branch)
	}

	reencoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTripped webapiv1.SessionListResponse
	if err := json.Unmarshal(reencoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal re-encoded: %v", err)
	}
	if roundTripped.Count != decoded.Count {
		t.Errorf("round-tripped Count = %d, want %d", roundTripped.Count, decoded.Count)
	}
}

// SessionDetail is built by TypeSpec composition (`extends SessionIdentity`
// plus a `...SessionRuntime` spread), which the OpenAPI3 emitter represents
// as allOf. oapi-codegen's types-only generation flattens that allOf into a
// single Go struct — this test is the verification the schema-contract ADR
// asks for: confirm the flattening actually carries every field from both
// sides, not just the fields declared directly on SessionDetail.
func TestRoundTrip_SessionDetailComposition(t *testing.T) {
	raw := readTestdata(t, "session_detail.valid.json")

	var got webapiv1.SessionDetail
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// From SessionIdentity:
	if got.SessionName != "team/workspace-a" || got.Workflow == nil || *got.Workflow != "default" {
		t.Errorf("identity fields missing after allOf flattening: %+v", got)
	}
	// From SessionRuntime:
	if got.Run != webapiv1.Up || !got.WorkspaceDirExists {
		t.Errorf("runtime fields missing after allOf flattening: %+v", got)
	}
}

// The `inputs` field is arbitrary user-defined JSON (Record<unknown> in
// TypeSpec, map[string]interface{} in Go). encoding/json decodes every
// number in that map as float64 — including one already past
// Number.MAX_SAFE_INTEGER (2^53-1) — because there is no schema telling the
// decoder any given key holds an integer. This is the same representation
// JavaScript's own JSON.parse uses for an untyped object, so Go and
// TypeScript lose precision identically rather than one silently
// disagreeing with the other; per the schema-contract ADR, that existing,
// shared limitation is not a reason to add a custom numeric decorator to
// this field without an actual affected consumer.
func TestRoundTrip_DynamicInputsJSON_LargeIntegerLosesPrecisionLikeJSNumber(t *testing.T) {
	raw := readTestdata(t, "session_detail.valid.json")

	var got webapiv1.SessionDetail
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Inputs == nil {
		t.Fatal("Inputs = nil, want the fixture's map")
	}
	in := *got.Inputs
	if in["reviewer"] != "alice" {
		t.Errorf(`Inputs["reviewer"] = %v, want alice`, in["reviewer"])
	}
	// A safe integer round-trips exactly.
	if in["retries"] != float64(3) {
		t.Errorf(`Inputs["retries"] = %v, want 3`, in["retries"])
	}
	// 9007199254740993 (2^53+1) is not representable as float64; it decodes
	// to the nearest representable value, 9007199254740992 (2^53) — this
	// assertion documents that rounding rather than hiding it.
	got9007 := in["budget_bytes"]
	want := float64(9007199254740992)
	if got9007 != want {
		t.Errorf(`Inputs["budget_bytes"] = %v, want %v (float64 rounding of 2^53+1)`, got9007, want)
	}
}

// encoding/json's Unmarshal does not enforce OpenAPI's `required` list —
// nothing in oapi-codegen's types-only generation adds runtime schema
// validation. A payload missing a required field decodes without error and
// leaves the field at its Go zero value; enum-typed fields expose exactly
// this gap through their generated Valid() method, which is how a handler
// is expected to catch it explicitly (the schema-contract ADR calls this
// "runtime validation ... remain explicit").
func TestRoundTrip_MissingRequiredFieldDecodesToZeroValueNotAnError(t *testing.T) {
	raw := readTestdata(t, "session_detail.invalid_missing_run.json")

	var got webapiv1.SessionDetail
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal unexpectedly failed: %v (documents a change in behavior worth investigating)", err)
	}
	if got.Run != "" {
		t.Fatalf("Run = %q, want the zero value (missing from the fixture)", got.Run)
	}
	if got.Run.Valid() {
		t.Error("Run.Valid() = true for the zero value, want false — this is the check a handler must run explicitly")
	}
}

// An explicit JSON `null` for an optional field is not the same wire event
// as omitting the key, even though this contract only ever produces the
// latter (web/api/README.md documents that). A conformant payload should
// never send null, but nothing stops one from doing so, and Go's decoder
// treats it identically to absence for every pointer-typed optional field
// here: the field ends up nil either way, which is the finding this test
// records rather than assumes.
func TestRoundTrip_ExplicitNullOptionalFieldsDecodeSameAsAbsent(t *testing.T) {
	raw := readTestdata(t, "session_detail.explicit_null.json")

	var got webapiv1.SessionDetail
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal explicit null: %v", err)
	}
	if got.ResourceId != nil {
		t.Errorf("ResourceId = %v, want nil for an explicit null", got.ResourceId)
	}
	if got.Branch != nil {
		t.Errorf("Branch = %v, want nil for an explicit null", got.Branch)
	}
	if got.Children != nil {
		t.Errorf("Children = %v, want nil for an explicit null", got.Children)
	}
	if got.Inputs != nil {
		t.Errorf("Inputs = %v, want nil for an explicit null", got.Inputs)
	}
	if got.Message != nil {
		t.Errorf("Message = %v, want nil for an explicit null", got.Message)
	}
	// Required fields alongside the explicit nulls still decode normally —
	// null on one field does not corrupt sibling fields.
	if got.SessionName != "team/workspace-a" || got.Run != webapiv1.Up {
		t.Errorf("required fields not carried through alongside explicit nulls: %+v", got)
	}
}

// DecodeApiError is the tagged union's read side: given a category it
// recognizes, it must decode into the matching concrete leaf type with the
// leaf's own narrower `code` enum populated.
func TestRoundTrip_ApiErrorTaggedUnion_KnownCategories(t *testing.T) {
	notFound, err := DecodeApiError(readTestdata(t, "error_not_found.json"))
	if err != nil {
		t.Fatalf("DecodeApiError(not_found): %v", err)
	}
	nf, ok := notFound.(webapiv1.NotFoundError)
	if !ok || nf.Code != webapiv1.SessionNotFound {
		t.Errorf("notFound = %#v, want a NotFoundError with code session_not_found", notFound)
	}

	conflict, err := DecodeApiError(readTestdata(t, "error_conflict.json"))
	if err != nil {
		t.Fatalf("DecodeApiError(conflict): %v", err)
	}
	c, ok := conflict.(webapiv1.ConflictError)
	if !ok || c.Code != webapiv1.HasChildren {
		t.Errorf("conflict = %#v, want a ConflictError with code has_children", conflict)
	}
}

// A category this contract's Go side does not know about must fail loudly
// rather than silently decode into the wrong leaf type or a zero-valued one.
func TestRoundTrip_ApiErrorTaggedUnion_UnknownCategoryIsRejected(t *testing.T) {
	_, err := DecodeApiError(readTestdata(t, "error_unknown_category.json"))
	if err == nil {
		t.Fatal("DecodeApiError(unknown category) = nil error, want a rejection")
	}
}
