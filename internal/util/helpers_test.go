package util

import (
	"orchids-api/internal/testutil"
	"testing"
)

func TestUniqueStrings(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "empty slice",
			input:    []string{},
			expected: nil,
		},
		{
			name:     "no duplicates",
			input:    []string{"a", "b", "c"},
			expected: []string{"a", "b", "c"},
		},
		{
			name:     "with duplicates",
			input:    []string{"a", "b", "a", "c", "b"},
			expected: []string{"a", "b", "c"},
		},
		{
			name:     "with empty strings",
			input:    []string{"a", "", "b", "", "c"},
			expected: []string{"a", "b", "c"},
		},
		{
			name:     "with whitespace",
			input:    []string{" a ", "a", "  b  ", "b"},
			expected: []string{"a", "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := UniqueStrings(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("expected length %d, got %d", len(tt.expected), len(result))
				return
			}
			for i := range result {
				testutil.CheckEqual(t, result[i], tt.expected[i])
			}
		})
	}
}

func TestSecureCompare(t *testing.T) {
	testutil.False(t, !SecureCompare("secret", "secret"), "equal values should match")
	testutil.False(t, SecureCompare("secret", "different"), "different values should not match")
}
