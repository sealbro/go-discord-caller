package ally

import "testing"

func TestNormalizeCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want Code
		ok   bool
	}{
		{"ABCD2345", "ABCD2345", true},
		{"  abcd2345\n", "ABCD2345", true},
		{"ABCD234", "", false},
		{"ABCD23456", "", false},
		{"ABCD1234", "", false}, // 1 is not in the base32 alphabet
		{"ABCD8901", "", false},
		{"ABCD-345", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := NormalizeCode(tt.raw)
		if got != tt.want || ok != tt.ok {
			t.Errorf("NormalizeCode(%q) = (%q, %v), want (%q, %v)", tt.raw, got, ok, tt.want, tt.ok)
		}
	}
}

// The store is the only issuer of relay codes, so every code it can produce has
// to pass the shape check that gates /start.
func TestNormalizeCodeAcceptsEveryGeneratedShape(t *testing.T) {
	t.Parallel()
	for _, code := range []Code{"AAAAAAAA", "ZZZZZZZZ", "22222222", "77777777", "A2Z7A2Z7"} {
		if _, ok := NormalizeCode(code); !ok {
			t.Errorf("NormalizeCode(%q) rejected a code with the generator's alphabet", code)
		}
	}
}
