package guild

import (
	"encoding/base64"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

func TestBotUserID(t *testing.T) {
	t.Parallel()
	encoded := base64.RawURLEncoding.EncodeToString([]byte("701"))

	tests := []struct {
		name   string
		token  string
		wantID snowflake.ID
		wantOK bool
	}{
		{"valid token", encoded + ".abc.def", 701, true},
		{"no dot", "nodots", 0, false},
		{"leading dot", ".abc.def", 0, false},
		{"not base64", "!!!.abc.def", 0, false},
		{"not a snowflake", base64.RawURLEncoding.EncodeToString([]byte("zzz")) + ".a.b", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id, ok := BotUserID(tc.token)
			if id != tc.wantID || ok != tc.wantOK {
				t.Errorf("BotUserID = (%v, %v), want (%v, %v)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}
