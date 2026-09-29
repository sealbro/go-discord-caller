package telemetry

import (
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

func TestParseVoiceRegion(t *testing.T) {
	tests := map[string]string{
		// Observed on a live raid, 2026-09-29: a "c-" prefix, the region and
		// its server number, then a hex instance id.
		"c-ams18-50a8bfa.discord.media:443": "c-ams",
		"c-ams13-8e0de.discord.media:443":   "c-ams",
		"c-ams22-1fa39d.discord.media:443":  "c-ams",

		// Constructed, not observed: a region whose own name contains a
		// hyphen must survive intact.
		"c-us-east18-50a8bfa.discord.media:443": "c-us-east",

		// Older name-plus-digits endpoints. Not seen in the wild since the
		// "c-" format appeared; kept so the fallback branch stays covered.
		"rotterdam9231.discord.media:443": "rotterdam",
		"us-east4823.discord.media:443":   "us-east",

		"": "",
	}

	for endpoint, want := range tests {
		if got := ParseVoiceRegion(endpoint); got != want {
			t.Errorf("ParseVoiceRegion(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

func TestVoiceRegionMetricsTracksAndForgets(t *testing.T) {
	var m VoiceRegionMetrics
	m.regions = make(map[voiceRegionKey]string)

	guildID, botID := snowflake.ID(1), snowflake.ID(2)
	key := voiceRegionKey{guildID: guildID, botID: botID}

	m.SetRegion(guildID, botID, "c-ams18-50a8bfa.discord.media:443")
	if got := m.regions[key]; got != "c-ams" {
		t.Fatalf("after SetRegion: got %q, want %q", got, "c-ams")
	}

	// A reconnect to a different media server in the same region must not
	// produce a second series.
	m.SetRegion(guildID, botID, "c-ams22-1fa39d.discord.media:443")
	if len(m.regions) != 1 {
		t.Fatalf("reconnect within a region: got %d series, want 1", len(m.regions))
	}

	// An unparseable endpoint leaves the last known region in place.
	m.SetRegion(guildID, botID, "")
	if got := m.regions[key]; got != "c-ams" {
		t.Fatalf("after empty endpoint: got %q, want %q", got, "c-ams")
	}

	m.ForgetRegion(guildID, botID)
	if len(m.regions) != 0 {
		t.Fatalf("after ForgetRegion: got %d series, want 0", len(m.regions))
	}
}
