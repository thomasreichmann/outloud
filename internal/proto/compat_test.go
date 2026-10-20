package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// These tests pin the wire format. Every 1.x release must talk to every
// other, so what one writes has to stay what the others read. Change an
// expectation here only with a new protocol Version. See COMPATIBILITY.md.

func TestConstants(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"Magic", Magic, "OUT2"},
		{"MagicV1", MagicV1, "OUT1"},
		{"Version", Version, 2},
		{"ServiceType", ServiceType, "_outloud._tcp"},
		{"DefaultPort", DefaultPort, 47811},
		{"MaxPayload", MaxPayload, 1 << 20},
		{"CaptureTXT", CaptureTXT, "capture=1"},
		{"VersionTXT", VersionTXT, "v=2"},
		{"RefusedNotPaired", RefusedNotPaired, "not paired"},
		{"RefusedNoCapture", RefusedNoCapture, "cannot capture"},
		{"RefusedBusy", RefusedBusy, "busy"},
		{"RefusedInUse", RefusedInUse, "in use"},
		{"RefusedVersion", RefusedVersion, "version"},
		{"CaptureNeedsPermission", CaptureNeedsPermission, "needs permission"},
		{"CaptureDenied", CaptureDenied, "denied"},
	} {
		if c.got != c.want {
			t.Errorf("%s is %v, want %v", c.name, c.got, c.want)
		}
	}
	types := []MsgType{MsgHello, MsgFormat, MsgAudio, MsgIdle, MsgVolume, MsgPing, MsgStats,
		MsgPairCommit, MsgPairNonce, MsgPairReveal, MsgPairDecision, MsgCapture}
	for i, typ := range types {
		if int(typ) != i+1 {
			t.Errorf("%v is %d, want %d", typ, typ, i+1)
		}
	}
}

func TestFraming(t *testing.T) {
	var b bytes.Buffer
	if err := WriteMsg(&b, MsgVolume, Volume{Gain: 0.5}.Encode()); err != nil {
		t.Fatal(err)
	}
	if err := WriteMsg(&b, MsgFormat, Format{SampleRate: 48000, Channels: 2}.Encode()); err != nil {
		t.Fatal(err)
	}
	if err := WriteMsg(&b, MsgPing, nil); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		5, 4, 0, 0, 0, 0x00, 0x00, 0x00, 0x3f, // volume 0.5, a float32
		2, 5, 0, 0, 0, 0x80, 0xbb, 0x00, 0x00, 2, // 48000 Hz, 2 channels
		6, 0, 0, 0, 0, // ping
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Errorf("wrote % x\nwant  % x", b.Bytes(), want)
	}
}

// sameJSON checks that v encodes to the fields in want, in any order.
func sameJSON(t *testing.T, v any, want string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var got, exp map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, exp) {
		t.Errorf("encoded %s\nwant    %s", b, want)
	}
}

func TestJSONFields(t *testing.T) {
	sameJSON(t, Hello{Version: 2, Name: "PC", Software: "Outloud 1.0.0", LatencyMs: 80,
		Listen: true, Pair: true, IfFree: true, Refused: RefusedInUse, User: "Other PC"},
		`{"version":2,"name":"PC","software":"Outloud 1.0.0","latency_ms":80,
		"listen":true,"pair":true,"if_free":true,"refused":"in use","user":"Other PC"}`)
	// Optional fields are left out, so a peer that knows none of them reads
	// the same Hello.
	sameJSON(t, Hello{Version: 2, Name: "PC"}, `{"version":2,"name":"PC"}`)
	sameJSON(t, Stats{State: "playing", BufferMs: 41.5, TargetMs: 40, DriftPPM: -3.25, Underruns: 1, Skips: 2,
		Output: "MacBook Pro Speakers", Muted: true, OutputError: "no device"},
		`{"state":"playing","buffer_ms":41.5,"target_ms":40,"drift_ppm":-3.25,"underruns":1,"skips":2,
		"output":"MacBook Pro Speakers","muted":true,"output_error":"no device"}`)
	sameJSON(t, Capture{Problem: CaptureDenied, Error: "not allowed"}, `{"problem":"denied","error":"not allowed"}`)
	sameJSON(t, Capture{Error: "no device"}, `{"error":"no device"}`)
}

// TestNewerPeers checks that messages from a later 1.x release, with fields
// and values this one does not know, still read.
func TestNewerPeers(t *testing.T) {
	var b bytes.Buffer
	WriteMsg(&b, MsgHello, []byte(`{"version":2,"name":"Mac","software":"Outloud 1.9.0",
		"refused":"some new reason","features":["x"],"limits":{"rate":96000}}`))
	h, err := NewReader(&b).ReadHello()
	if err != nil || h.Name != "Mac" || h.Refused != "some new reason" {
		t.Errorf("read %+v, %v", h, err)
	}
	var st Stats
	if err := json.Unmarshal([]byte(`{"state":"playing","target_ms":40,"latency_ms":12,"codec":"opus"}`), &st); err != nil || st.TargetMs != 40 {
		t.Errorf("read %+v, %v", st, err)
	}
}

func TestVersionMismatch(t *testing.T) {
	var b bytes.Buffer
	WriteMsg(&b, MsgHello, []byte(`{"version":3,"name":"Future Mac","refused":"version"}`))
	h, err := NewReader(&b).ReadHello()
	var verr *VersionError
	if !errors.As(err, &verr) || verr.Peer != 3 {
		t.Fatalf("got %v", err)
	}
	if h.Name != "Future Mac" || h.Refused != RefusedVersion {
		t.Errorf("hello %+v", h)
	}
}

func TestTXTVersion(t *testing.T) {
	for _, c := range []struct {
		txt  []string
		want int
	}{
		{[]string{"v=2", "capture=1"}, 2},
		{[]string{"capture=1", "v=3"}, 3},
		{nil, 1},
		{[]string{"v=x"}, 1},
	} {
		if got := TXTVersion(c.txt); got != c.want {
			t.Errorf("TXTVersion(%q) = %d, want %d", c.txt, got, c.want)
		}
	}
}
