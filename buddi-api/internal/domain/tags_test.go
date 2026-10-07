package domain_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// TestTagListRoundTrip is the test the live smoke run failed without: tags go
// out as a text[] literal and come back unchanged.
func TestTagListRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		tags []string
	}{
		{name: "nil", tags: nil},
		{name: "empty", tags: []string{}},
		{name: "one", tags: []string{"work"}},
		{name: "several", tags: []string{"home", "work"}},
		{name: "with space", tags: []string{"deep work"}},
		{name: "with comma", tags: []string{"a,b"}},
		{name: "with braces", tags: []string{"{weird}"}},
		{name: "with quote", tags: []string{`say "hi"`}},
		{name: "with backslash", tags: []string{`back\slash`}},
		{name: "the word null", tags: []string{"null", "NULL"}},
		{name: "empty element", tags: []string{"", "work"}},
		{name: "unicode", tags: []string{"café", "日本語"}},
		{name: "everything", tags: []string{`{a,"b,c"}`, "d e", "", "null"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list := domain.TagListOf(tc.tags)

			got, err := list.Slice()
			if err != nil {
				t.Fatalf("Slice: %v", err)
			}

			want := tc.tags
			if len(want) == 0 {
				want = []string{}
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip of %v via %q gave %v", want, list, got)
			}
		})
	}
}

// A nil tag list must serialise as the empty array, not NULL, because the column
// is NOT NULL.
func TestTagListNilIsEmptyArrayNotNull(t *testing.T) {
	value, err := domain.TagListOf(nil).Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}

	if value != "{}" {
		t.Errorf("Value = %v, want {}", value)
	}
}

// The literal shape matters: it is what PostgreSQL parses on the way in.
func TestTagListLiteralFormat(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{tags: nil, want: "{}"},
		{tags: []string{"work"}, want: "{work}"},
		{tags: []string{"a", "b"}, want: "{a,b}"},
		{tags: []string{"a,b"}, want: `{"a,b"}`},
		{tags: []string{"deep work"}, want: `{"deep work"}`},
		{tags: []string{"null"}, want: `{"null"}`},
		{tags: []string{""}, want: `{""}`},
		{tags: []string{`a"b`}, want: `{"a\"b"}`},
	}

	for _, tc := range cases {
		got := string(domain.TagListOf(tc.tags))
		if got != tc.want {
			t.Errorf("TagListOf(%v) = %s, want %s", tc.tags, got, tc.want)
		}
	}
}

func TestTagListScanAcceptsStringBytesAndNull(t *testing.T) {
	for _, src := range []any{nil, "", "   ", "{work,home}", []byte("{work,home}")} {
		var got domain.TagList
		if err := got.Scan(src); err != nil {
			t.Errorf("Scan(%v): %v", src, err)
			continue
		}

		if src == nil || src == "" || src == "   " {
			if got.Len() != 0 {
				t.Errorf("Scan(%v) gave %q, want empty", src, got)
			}
		}
	}
}

func TestTagListScanRejectsGarbage(t *testing.T) {
	for _, literal := range []string{"not an array", "{unterminated"} {
		var got domain.TagList
		if err := got.Scan(literal); err != nil {
			continue
		}

		if _, err := got.Slice(); err == nil {
			t.Errorf("literal %q was accepted as a tag array", literal)
		}
	}
}

// The JSON shape must stay a plain array, so the storage format stays invisible
// to clients.
func TestTagListJSONIsAnArray(t *testing.T) {
	note := domain.Note{Tags: domain.TagListOf([]string{"home", "work"})}

	encoded, err := json.Marshal(note)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Tags []string `json:"tags"`
	}

	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if want := []string{"home", "work"}; !reflect.DeepEqual(decoded.Tags, want) {
		t.Errorf("tags over JSON = %v, want %v", decoded.Tags, want)
	}
}

func TestTagListJSONEmptyIsAnArrayNotNull(t *testing.T) {
	encoded, err := json.Marshal(domain.Note{Tags: domain.TagListOf(nil)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if want := `"tags":[]`; !contains(string(encoded), want) {
		t.Errorf("empty tags encoded as %s, want it to contain %s", encoded, want)
	}
}

func TestTagListUnmarshalJSON(t *testing.T) {
	var list domain.TagList
	if err := json.Unmarshal([]byte(`["Home","work","work"]`), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Deliberately lossless: lowercasing, trimming and de-duplicating are the
	// service's job via NormaliseTags, and doing them here as well would put the
	// same policy in two layers.
	got, err := list.Slice()
	if err != nil {
		t.Fatalf("Slice: %v", err)
	}

	if want := []string{"Home", "work", "work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}

	return -1
}
