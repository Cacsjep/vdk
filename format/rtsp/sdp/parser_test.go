package sdp

import (
	"strings"
	"testing"
)

const sampleVideoAudioSDP = `v=0
o=- 1459325504777324 1 IN IP4 192.168.0.123
s=RTSP/RTP stream from Network Video Server
t=0 0
a=control:*
m=video 0 RTP/AVP 96
a=rtpmap:96 H264/90000
a=fmtp:96 packetization-mode=1; sprop-parameter-sets=Z00AHpWoKA9k,aO48gA==
a=control:track1
m=audio 0 RTP/AVP 96
a=rtpmap:96 MPEG4-GENERIC/16000/2
a=control:track2
`

const sampleApplicationSDP = `v=0
o=- 17021470917246897137 1 IN IP4 192.168.2.90
s=Session streamed with GStreamer
t=0 0
a=control:rtsp://cam/axis-media/media.amp?video=0&audio=0&ptz=all
m=application 0 RTP/AVP 98
c=IN IP4 0.0.0.0
a=rtpmap:98 vnd.onvif.metadata/90000
a=control:rtsp://cam/axis-media/media.amp/stream=0?video=0&audio=0&ptz=all
`

const sampleMetadataSDP = `v=0
o=- 1 1 IN IP4 192.168.2.90
s=Session
t=0 0
m=metadata 0 RTP/AVP 100
a=rtpmap:100 METADATA/90000
a=control:track1
`

const sampleMixedSDP = `v=0
o=- 1 1 IN IP4 192.168.2.90
s=Session
t=0 0
m=video 0 RTP/AVP 96
a=rtpmap:96 H264/90000
a=control:track1
m=application 0 RTP/AVP 98
a=rtpmap:98 vnd.onvif.metadata/90000
a=control:track2
`

func TestParseVideoAudio(t *testing.T) {
	_, medias := Parse(sampleVideoAudioSDP)
	if len(medias) != 2 {
		t.Fatalf("expected 2 medias, got %d", len(medias))
	}
	if medias[0].AVType != "video" {
		t.Errorf("medias[0].AVType: want video, got %q", medias[0].AVType)
	}
	if medias[1].AVType != "audio" {
		t.Errorf("medias[1].AVType: want audio, got %q", medias[1].AVType)
	}
}

// TestParseApplicationMetadataTrack covers the fork's addition: SDP m=application
// (the canonical ONVIF spelling used by Axis) must produce a Media entry, not
// be silently discarded.
func TestParseApplicationMetadataTrack(t *testing.T) {
	_, medias := Parse(sampleApplicationSDP)
	if len(medias) != 1 {
		t.Fatalf("expected 1 media for application SDP, got %d", len(medias))
	}
	m := medias[0]
	if m.AVType != "application" {
		t.Errorf("AVType: want application, got %q", m.AVType)
	}
	if m.PayloadType != 98 {
		t.Errorf("PayloadType: want 98, got %d", m.PayloadType)
	}
	if !strings.Contains(m.Control, "stream=0") {
		t.Errorf("Control: want URL ending stream=0, got %q", m.Control)
	}
}

// TestParseMetadataKeywordTrack covers the alternative m=metadata spelling seen
// on some Axis firmware versions.
func TestParseMetadataKeywordTrack(t *testing.T) {
	_, medias := Parse(sampleMetadataSDP)
	if len(medias) != 1 {
		t.Fatalf("expected 1 media for metadata SDP, got %d", len(medias))
	}
	if medias[0].AVType != "metadata" {
		t.Errorf("AVType: want metadata, got %q", medias[0].AVType)
	}
	if medias[0].PayloadType != 100 {
		t.Errorf("PayloadType: want 100, got %d", medias[0].PayloadType)
	}
}

// TestParseMixedVideoAndMetadata ensures application tracks coexist with video
// tracks in a single SDP and ordering is preserved.
func TestParseMixedVideoAndMetadata(t *testing.T) {
	_, medias := Parse(sampleMixedSDP)
	if len(medias) != 2 {
		t.Fatalf("expected 2 medias for mixed SDP, got %d", len(medias))
	}
	if medias[0].AVType != "video" {
		t.Errorf("medias[0].AVType: want video, got %q", medias[0].AVType)
	}
	if medias[1].AVType != "application" {
		t.Errorf("medias[1].AVType: want application, got %q", medias[1].AVType)
	}
}
