//go:build windows

package main

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"testing"
)

// TestConfigFormat checks that settings saved by earlier releases still
// load, and that saving keeps their field names. Fields may be added, never
// renamed or given another meaning. See COMPATIBILITY.md.
func TestConfigFormat(t *testing.T) {
	for _, c := range []struct {
		name, file string
		want       Config
	}{
		{"0.1", `{"receiver": "Kitchen Mac", "address": "192.168.1.20:47811", "device": "Speakers (Realtek)",
			"latency_ms": 200, "forward_volume": true}`,
			Config{Receiver: "Kitchen Mac", Address: "192.168.1.20:47811", Device: "Speakers (Realtek)", LatencyMs: 200, ForwardVolume: true}},
		{"0.2, hearing", `{"receiver": "", "latency_ms": 40, "forward_volume": false,
			"hear": "Kitchen Mac", "hear_address": "192.168.1.20:47811"}`,
			Config{LatencyMs: 40, Hear: "Kitchen Mac", HearAddress: "192.168.1.20:47811"}},
		{"a later release", `{"receiver": "Kitchen Mac", "latency_ms": 80, "forward_volume": false, "something_new": [1, 2]}`,
			Config{Receiver: "Kitchen Mac", LatencyMs: 80}},
		{"no latency", `{"receiver": "Kitchen Mac"}`, Config{Receiver: "Kitchen Mac", LatencyMs: 80}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("APPDATA", t.TempDir())
			if err := os.MkdirAll(dataDir("APPDATA"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath(), []byte(c.file), 0o644); err != nil {
				t.Fatal(err)
			}
			got, saved := loadConfig()
			if !saved || got != c.want {
				t.Errorf("loaded %+v, %v\nwant   %+v", got, saved, c.want)
			}
		})
	}

	t.Setenv("APPDATA", t.TempDir())
	all := Config{Receiver: "r", Address: "a", Device: "d", LatencyMs: 1, ForwardVolume: true, Hear: "h", HearAddress: "ha"}
	if err := saveConfig(all); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(fields))
	want := []string{"address", "device", "forward_volume", "hear", "hear_address", "latency_ms", "receiver"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("saved the fields %v, want %v", keys, want)
	}
	if got, _ := loadConfig(); got != all {
		t.Errorf("read back %+v", got)
	}
}
