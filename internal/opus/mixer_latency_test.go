package opus

import (
	"testing"
	"time"
)

// An unstamped frame in slot 0 must not suppress the observation for the
// frames that do carry a timestamp: recordPipelineLatency skips the whole
// tick when oldestCreatedAt returns the zero Time.
func TestOldestCreatedAtSkipsUnstampedFirstFrame(t *testing.T) {
	base := time.Now().Add(-50 * time.Millisecond)

	tests := []struct {
		name   string
		frames []Frame
		want   time.Time
	}{
		{
			name:   "all stamped picks earliest",
			frames: []Frame{{CreatedAt: base.Add(20 * time.Millisecond)}, {CreatedAt: base}},
			want:   base,
		},
		{
			name:   "unstamped first frame",
			frames: []Frame{{}, {CreatedAt: base}},
			want:   base,
		},
		{
			name:   "unstamped later frame",
			frames: []Frame{{CreatedAt: base}, {}},
			want:   base,
		},
		{
			name:   "none stamped",
			frames: []Frame{{}, {}},
			want:   time.Time{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := oldestCreatedAt(tc.frames); !got.Equal(tc.want) {
				t.Fatalf("oldestCreatedAt = %v, want %v", got, tc.want)
			}
		})
	}
}
