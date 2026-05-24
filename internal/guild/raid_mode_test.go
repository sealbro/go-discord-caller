package guild

import "testing"

func TestRaidMode_WithCapture(t *testing.T) {
	cases := []struct {
		mode RaidMode
		want bool
	}{
		{RaidModeOneCaller, false},
		{RaidModeGuildCaller, true},
		{RaidModeAllyListener, false},
		{RaidModeAllyCaller, true},
		{RaidModeOneManyGuildCaller, true},
		{RaidModeOneManyAllyCaller, true},
	}
	for _, tc := range cases {
		if got := tc.mode.WithCapture(); got != tc.want {
			t.Errorf("%s.WithCapture() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestRaidMode_AllowGuestCapture(t *testing.T) {
	cases := []struct {
		mode RaidMode
		want bool
	}{
		{RaidModeOneCaller, false},
		{RaidModeGuildCaller, true},
		{RaidModeAllyListener, false},
		{RaidModeAllyCaller, false},
		{RaidModeOneManyGuildCaller, true},
		{RaidModeOneManyAllyCaller, false},
	}
	for _, tc := range cases {
		if got := tc.mode.AllowGuestCapture(); got != tc.want {
			t.Errorf("%s.AllowGuestCapture() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestRaidMode_IsStarTopology(t *testing.T) {
	cases := []struct {
		mode RaidMode
		want bool
	}{
		{RaidModeOneCaller, false},
		{RaidModeGuildCaller, false},
		{RaidModeAllyListener, false},
		{RaidModeAllyCaller, false},
		{RaidModeOneManyGuildCaller, true},
		{RaidModeOneManyAllyCaller, true},
	}
	for _, tc := range cases {
		if got := tc.mode.IsStarTopology(); got != tc.want {
			t.Errorf("%s.IsStarTopology() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestRaidMode_IsDirectPassthrough(t *testing.T) {
	cases := []struct {
		mode RaidMode
		want bool
	}{
		{RaidModeOneCaller, true},
		{RaidModeGuildCaller, false},
		{RaidModeAllyListener, false},
		{RaidModeAllyCaller, false},
		{RaidModeOneManyGuildCaller, false},
		{RaidModeOneManyAllyCaller, false},
	}
	for _, tc := range cases {
		if got := tc.mode.IsDirectPassthrough(); got != tc.want {
			t.Errorf("%s.IsDirectPassthrough() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestRaidMode_IsDirectOutput(t *testing.T) {
	cases := []struct {
		mode RaidMode
		want bool
	}{
		{RaidModeOneCaller, true},
		{RaidModeGuildCaller, false},
		{RaidModeAllyListener, false},
		{RaidModeAllyCaller, false},
		{RaidModeOneManyGuildCaller, false},
		{RaidModeOneManyAllyCaller, true},
	}
	for _, tc := range cases {
		if got := tc.mode.IsDirectOutput(); got != tc.want {
			t.Errorf("%s.IsDirectOutput() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestRaidMode_Pretty(t *testing.T) {
	cases := []struct {
		mode RaidMode
		want string
	}{
		{RaidModeOneCaller, "One Caller (host)"},
		{RaidModeGuildCaller, "Many Callers (host)"},
		{RaidModeAllyListener, "Listener (guest)"},
		{RaidModeAllyCaller, "Caller (guest)"},
		{RaidModeOneManyGuildCaller, "One↔Many Callers (host)"},
		{RaidModeOneManyAllyCaller, "One↔Many Caller (guest)"},
		{RaidMode("unknown"), "unknown"},
	}
	for _, tc := range cases {
		// nil translator → English fallback (matches en.yaml).
		if got := tc.mode.Pretty(nil); got != tc.want {
			t.Errorf("%s.Pretty(nil) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}
