package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jianyuan/terraform-provider-sentry/internal/apiclient"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestNewOptionalTicketString(t *testing.T) {
	// Sentry reports an unset ticket field either by omitting the key or by
	// returning an empty string. Both must normalise to null, otherwise an
	// attribute the user never set shows permanent drift after apply.
	for _, tt := range []struct {
		name     string
		value    *string
		wantNull bool
		wantStr  string
	}{
		{name: "absent key", value: nil, wantNull: true},
		{name: "empty string", value: ptr(""), wantNull: true},
		{name: "set value", value: ptr("oncall,triage"), wantStr: "oncall,triage"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := newOptionalTicketString(tt.value)
			if tt.wantNull {
				if !got.IsNull() {
					t.Fatalf("expected null, got %q", got.Get())
				}
				return
			}
			if got.IsNull() {
				t.Fatal("expected a value, got null")
			}
			if got.Get() != tt.wantStr {
				t.Fatalf("expected %q, got %q", tt.wantStr, got.Get())
			}
		})
	}
}

func TestNewOptionalTicketStringSet(t *testing.T) {
	ctx := context.Background()

	if got := newOptionalTicketStringSet(ctx, nil); !got.IsNull() {
		t.Fatal("expected null for an absent components key")
	}

	if got := newOptionalTicketStringSet(ctx, &[]string{}); !got.IsNull() {
		t.Fatal("expected null for an empty components list")
	}

	got := newOptionalTicketStringSet(ctx, &[]string{"10001", "10002"})
	if got.IsNull() {
		t.Fatal("expected a value for a populated components list")
	}
	elems, diags := got.Get(ctx)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(elems) != 2 {
		t.Fatalf("expected 2 components, got %d", len(elems))
	}
}

func ptr(s string) *string { return &s }

func TestCamelToSnakeCase(t *testing.T) {
	// Mirrors Django's re_camel_case, which Sentry applies to every incoming
	// payload key. Used to tell the user what their key would be turned into.
	for in, want := range map[string]string{
		"fixVersions":       "fix_versions",
		"dueDate":           "due_date",
		"lastViewed":        "last_viewed",
		"FixVersions":       "fix_versions",
		"FIXVERSIONS":       "fixversions",
		"labels":            "labels",
		"customfield_10101": "customfield_10101",
	} {
		if got := camelToSnakeCase(in); got != want {
			t.Errorf("camelToSnakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTicketAdditionalFieldKeyValidator(t *testing.T) {
	ctx := context.Background()
	v := ticketAdditionalFieldKeyValidator()

	for _, tt := range []struct {
		key     string
		wantErr bool
	}{
		// Keys that survive Sentry's conversion unchanged.
		{key: "customfield_10101"},
		{key: "fixversions"},
		{key: "duedate"},
		// Pure case changes are harmless: Sentry stores "upper", and Jira
		// lowercases both sides when matching, so it resolves to the same
		// field. Verified against the live API.
		{key: "UPPER"},
		{key: "Environment"},
		// An inserted underscore does break the field.
		{key: "fixVersions", wantErr: true},
		{key: "dueDate", wantErr: true},
		{key: "MixedCase", wantErr: true},
		// Reserved: has a dedicated attribute, or is set by Sentry itself.
		{key: "labels", wantErr: true},
		{key: "project", wantErr: true},
		{key: "description", wantErr: true},
		{key: "dynamic_form_fields", wantErr: true},
		// Reserved keys must be caught in their post-conversion form too --
		// "Labels" is stored as "labels" and would collide with the dedicated
		// attribute just the same.
		{key: "Labels", wantErr: true},
		{key: "PRIORITY", wantErr: true},
	} {
		t.Run(tt.key, func(t *testing.T) {
			req := validator.StringRequest{
				Path:        path.Root("additional_fields"),
				ConfigValue: types.StringValue(tt.key),
			}
			resp := &validator.StringResponse{}
			v.ValidateString(ctx, req, resp)

			if got := resp.Diagnostics.HasError(); got != tt.wantErr {
				t.Fatalf("key %q: HasError() = %t, want %t (%v)", tt.key, got, tt.wantErr, resp.Diagnostics)
			}
		})
	}
}

// TestTicketAdditionalFieldsOverwriteTypedAttribute documents why the reserved
// key check is needed independently of Sentry's camelCase bug: the generated
// marshaller writes AdditionalProperties after the typed fields, into the same
// JSON object, so a passthrough key silently wins.
func TestTicketAdditionalFieldsOverwriteTypedAttribute(t *testing.T) {
	var action apiclient.OrganizationWorkflowActionFilterActionJira
	action.Data.AdditionalFields.Project = "10000"
	action.Data.AdditionalFields.Issuetype = "10001"
	labels := "oncall,triage"
	action.Data.AdditionalFields.Labels = &labels

	action.Data.AdditionalFields.Set("labels", "clobbered")

	encoded, err := json.Marshal(action.Data.AdditionalFields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded["labels"] != "clobbered" {
		t.Fatalf("expected the passthrough value to win (documenting the collision), got %v", decoded["labels"])
	}

	// The validator is what stops a user from reaching that state.
	resp := &validator.StringResponse{}
	ticketAdditionalFieldKeyValidator().ValidateString(
		context.Background(),
		validator.StringRequest{
			Path:        path.Root("additional_fields"),
			ConfigValue: types.StringValue("labels"),
		},
		resp,
	)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected the validator to reject a key that collides with a dedicated attribute")
	}
}

// TestNewAssignedToTargetIdentifier checks the JSON type of an `assigned_to`
// target id. The API accepts a numeric id sent as a string but never matches
// it against an assignee, and state records the same value for both since
// `target_id` is a string attribute either way.
func TestNewAssignedToTargetIdentifier(t *testing.T) {
	for _, tt := range []struct {
		name       string
		targetType apiclient.OrganizationWorkflowActionFilterConditionAssignedToComparisonTargetType
		targetID   string
		want       string
		wantErr    bool
	}{
		{name: "team", targetType: "Team", targetID: "4505449279324160", want: "4505449279324160"},
		{name: "member", targetType: "Member", targetID: "12345", want: "12345"},
		// Sentry has no string-identified assignee, so a non-numeric id is a
		// configuration error, reported rather than sent as a value that never
		// matches.
		{name: "team with a slug", targetType: "Team", targetID: "response", wantErr: true},
		// Unassigned has no target.
		{name: "unassigned", targetType: "Unassigned", want: `""`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newAssignedToTargetIdentifier(tt.targetType, tt.targetID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %s/%q", tt.targetType, tt.targetID)
				}
				return
			}
			if err != nil {
				t.Fatalf("newAssignedToTargetIdentifier(%s, %q): %v", tt.targetType, tt.targetID, err)
			}

			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			if string(encoded) != tt.want {
				t.Fatalf("targetIdentifier for %s/%q = %s, want %s", tt.targetType, tt.targetID, encoded, tt.want)
			}
		})
	}
}
