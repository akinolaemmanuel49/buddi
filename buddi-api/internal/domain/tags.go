package domain

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
)

// TagList is a set of labels stored in a PostgreSQL text[] column.
//
// It is backed by a string rather than a slice on purpose. GORM renders a slice
// field inline as a SQL tuple, so a []string column arrives as ('work','home')
// and the driver rejects it with "malformed array literal". A string field binds
// as a real parameter, and because the target column and the @> operator both
// declare text[], PostgreSQL resolves the parameter to text[] and parses the
// literal for us. So the format has to be visible in the type, and the
// slice-shaped helpers below keep it out of the callers.
//
// The wire format is PostgreSQL's text array literal. Tags are free text, so an
// element may contain a comma, a quote or a backslash, and a naive
// join-then-split would corrupt it.
type TagList string

// TagListOf builds a TagList from plain strings. A nil or empty input becomes
// the empty array rather than NULL, because the column is NOT NULL and "no
// tags" must not be storable as "unknown tags".
func TagListOf(tags []string) TagList {
	if len(tags) == 0 {
		return TagList("{}")
	}

	var b strings.Builder

	b.Grow(len(tags) * 8)
	b.WriteByte('{')

	for i, tag := range tags {
		if i > 0 {
			b.WriteByte(',')
		}

		writeArrayElement(&b, tag)
	}

	b.WriteByte('}')

	return TagList(b.String())
}

// Value satisfies driver.Valuer. GORM calls it so a bare TagList is usable as a
// query argument, and returning the literal keeps the type self-describing.
func (t TagList) Value() (driver.Value, error) {
	if strings.TrimSpace(string(t)) == "" {
		return "{}", nil
	}

	return string(t), nil
}

// Scan satisfies sql.Scanner, accepting the string and []byte forms the driver
// may hand over depending on how the value was fetched.
func (t *TagList) Scan(src any) error {
	switch value := src.(type) {
	case nil:
		*t = TagList("{}")

		return nil

	case string:
		if strings.TrimSpace(value) == "" {
			*t = TagList("{}")

			return nil
		}

		*t = TagList(value)

		return nil

	case []byte:
		return t.Scan(string(value))

	default:
		return fmt.Errorf("domain: cannot scan %T into tags", src)
	}
}

// Slice returns the tags as ordinary strings. A malformed literal yields the
// error, so a bad row is visible rather than silently becoming an empty tag set.
func (t TagList) Slice() ([]string, error) {
	return parseTextArray(string(t))
}

// MarshalJSON emits a plain JSON array, so the API surface is unaffected by the
// storage format.
func (t TagList) MarshalJSON() ([]byte, error) {
	tags, err := t.Slice()
	if err != nil {
		return nil, err
	}

	if tags == nil {
		tags = []string{}
	}

	return json.Marshal(tags)
}

// UnmarshalJSON accepts a JSON array so a client payload can be read into the
// same type.
func (t *TagList) UnmarshalJSON(data []byte) error {
	var tags []string

	if err := json.Unmarshal(data, &tags); err != nil {
		return err
	}

	*t = TagListOf(tags)

	return nil
}

// Len reports how many tags are stored.
func (t TagList) Len() int {
	tags, err := t.Slice()
	if err != nil {
		return 0
	}

	return len(tags)
}

// needsQuoting reports whether an element cannot appear bare in an array
// literal. An element that is the word NULL would otherwise be read back as SQL
// NULL rather than as the four letters, so it has to be quoted too.
func needsQuoting(value string) bool {
	if value == "" {
		return true
	}

	if strings.EqualFold(value, "null") {
		return true
	}

	return strings.ContainsAny(value, "{}\",\\\"\t\n\r ")
}

// writeArrayElement writes one element, quoting it only when it has to be.
func writeArrayElement(b *strings.Builder, value string) {
	if !needsQuoting(value) {
		b.WriteString(value)

		return
	}

	b.WriteByte('"')

	for _, r := range value {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}

	b.WriteByte('"')
}

// parseTextArray parses a PostgreSQL one-dimensional text array literal.
func parseTextArray(literal string) ([]string, error) {
	trimmed := strings.TrimSpace(literal)

	if trimmed == "" {
		return []string{}, nil
	}

	if trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return nil, fmt.Errorf("domain: %q is not a Postgres array literal", literal)
	}

	body := trimmed[1 : len(trimmed)-1]
	if strings.TrimSpace(body) == "" {
		return []string{}, nil
	}

	var (
		out     []string
		current strings.Builder
		quoted  bool
		escaped bool
	)

	flush := func() {
		out = append(out, current.String())
		current.Reset()
	}

	for i := 0; i < len(body); i++ {
		c := body[i]

		switch {
		case escaped:
			current.WriteByte(c)
			escaped = false

		case quoted && c == '\\':
			escaped = true

		case c == '"':
			quoted = !quoted

		case !quoted && c == ',':
			flush()

		case !quoted && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			// Whitespace outside quotes is insignificant padding around the
			// element, so it is dropped rather than becoming part of the tag.

		default:
			current.WriteByte(c)
		}
	}

	flush()

	return out, nil
}
