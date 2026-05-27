package rtspv2

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/deepch/vdk/av"
)

// ----------------------------------------------------------------------------
// Shared packet helpers and client constructors.
// ----------------------------------------------------------------------------

// makeInterleavedRTPPacket builds the raw bytes that arrive over a RTSP-over-TCP
// connection on the interleaved data channel: 4 bytes of interleaved framing
// followed by a 12-byte RTP header followed by the payload.
//
//	[0]:     0x24 (RTP-over-TCP magic, '$')
//	[1]:     interleaved channel id
//	[2:4]:   length BE (RTP header + payload)
//	[4]:     V=2 P=0 X=0 CC=0  => 0x80
//	[5]:     marker (bit 7) + payload type (bits 0..6)
//	[6:8]:   sequence number BE
//	[8:12]:  RTP timestamp BE
//	[12:16]: SSRC BE
//	[16:]:   RTP payload
func makeInterleavedRTPPacket(channel byte, marker bool, seq uint16, ts uint32, ssrc uint32, payload []byte) []byte {
	buf := make([]byte, 16+len(payload))
	buf[0] = 0x24
	buf[1] = channel
	binary.BigEndian.PutUint16(buf[2:4], uint16(12+len(payload)))
	buf[4] = 0x80
	buf[5] = 0x60 // arbitrary PT in dynamic range
	if marker {
		buf[5] |= 0x80
	}
	binary.BigEndian.PutUint16(buf[6:8], seq)
	binary.BigEndian.PutUint32(buf[8:12], ts)
	binary.BigEndian.PutUint32(buf[12:16], ssrc)
	copy(buf[16:], payload)
	return buf
}

// makeMetadataPacket is the metadata-channel convenience wrapper used by the
// metadata reassembly tests. SSRC is fixed; it is not part of the contract
// the demuxer cares about for metadata.
func makeMetadataPacket(channel byte, marker bool, seq uint16, ts uint32, payload []byte) []byte {
	return makeInterleavedRTPPacket(channel, marker, seq, ts, 0xDEADBEEF, payload)
}

// newTestMetadataClient builds a minimal RTSPClient wired so RTPDemuxer
// dispatches metadata packets on channel 4 to handleMetadata. No network.
func newTestMetadataClient(queueSize int) *RTSPClient {
	return &RTSPClient{
		videoID:               -1,
		audioID:               -2,
		metadataID:            4,
		OutgoingMetadataQueue: make(chan []byte, queueSize),
		BufferRtpPacket:       bytes.NewBuffer(nil),
	}
}

// newTestAudioClient builds an RTSPClient wired so RTPDemuxer dispatches audio
// packets on channel 6 through the AAC code path.
func newTestAudioClient() *RTSPClient {
	return &RTSPClient{
		videoID:         -1,
		audioID:         6,
		metadataID:      -3,
		audioCodec:      av.AAC,
		AudioTimeScale:  48000,
		BufferRtpPacket: bytes.NewBuffer(nil),
	}
}

// callDemuxerRecovering invokes RTPDemuxer and returns true if a panic occurred.
func callDemuxerRecovering(c *RTSPClient, pkt []byte) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	c.RTPDemuxer(&pkt)
	return false
}

// ----------------------------------------------------------------------------
// RTP header parsing.
// ----------------------------------------------------------------------------

// TestRTPDemuxer_TimestampIsExactly4Bytes is a regression guard. RFC 3550 §5.1
// defines the RTP timestamp as a 4-byte field at offset 4..7 of the RTP header.
// An earlier version of the demuxer sliced 8 bytes by mistake and only worked
// by accident because Uint32 ignored the trailing 4 bytes (the SSRC). If
// somebody widens that read window again, mutating the SSRC will leak into the
// parsed timestamp and this test will catch it.
func TestRTPDemuxer_TimestampIsExactly4Bytes(t *testing.T) {
	const wantTS uint32 = 0x11223344
	c := newTestMetadataClient(4)

	for _, ssrc := range []uint32{0x00000000, 0xDEADBEEF, 0xFFFFFFFF} {
		pkt := makeInterleavedRTPPacket(4, true, 1, wantTS, ssrc, []byte("x"))
		c.RTPDemuxer(&pkt)

		if got := uint32(c.timestamp); got != wantTS {
			t.Errorf("ssrc=%#x: parsed timestamp %#x, want %#x (demuxer leaked SSRC bytes)",
				ssrc, got, wantTS)
		}
		select {
		case <-c.OutgoingMetadataQueue:
		default:
		}
	}
}

// ----------------------------------------------------------------------------
// Audio path: AAC frame bounds.
// ----------------------------------------------------------------------------

// TestHandleAudio_AACMalformedPacketsDoNotPanic feeds the three classes of
// malformed AAC RTP packets that used to crash the process. The handler runs
// inside the goroutine spawned by Dial, so a panic there is unrecoverable from
// the caller.
func TestHandleAudio_AACMalformedPacketsDoNotPanic(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		// 1) NAL too short to even read the AU-headers length (< 2 bytes).
		{name: "short_nal", payload: []byte{0x00}},

		// 2) auHeadersLength = 0x40 claims 4 AU headers, so
		//    framesPayloadOffset = 2 + 4*2 = 10, but the payload is only
		//    5 bytes. The slice nal[2:framesPayloadOffset] would panic
		//    without the bounds check.
		{name: "framesPayloadOffset_overflow", payload: []byte{0x00, 0x40, 0x00, 0x00, 0x00}},

		// 3) Well-formed header for 1 AU header, but auHeader's frameSize
		//    (top 13 bits) is huge, far larger than the actual payload.
		//    framesPayload[:frameSize] would panic without the check.
		{name: "frameSize_overflow", payload: []byte{0x00, 0x10, 0xFF, 0xF8}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestAudioClient()
			pkt := makeInterleavedRTPPacket(6, true, 1, 1000, 0xCAFE, tc.payload)
			if callDemuxerRecovering(c, pkt) {
				t.Fatalf("malformed AAC packet panicked the demuxer")
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Video path: H.264 STAP-A bounds.
// ----------------------------------------------------------------------------

// newTestH264Client wires the RTSPClient so RTPDemuxer dispatches H.264 video
// packets on channel 8 through handleVideo / handleH264Payload.
func newTestH264Client() *RTSPClient {
	return &RTSPClient{
		videoID:         8,
		audioID:         -2,
		metadataID:      -3,
		videoCodec:      av.H264,
		BufferRtpPacket: bytes.NewBuffer(nil),
	}
}

// TestHandleH264_STAPAZeroSizeDoesNotPanic feeds a STAP-A aggregation packet
// whose first inner NAL declares size=0. The previous bounds check
// (size+2 > len(packet)) was insufficient because for size=0 the check is
// 2 > 2 (false), and the next line read packet[2] which is out of bounds.
// The handler runs in the Dial goroutine so the panic killed the process.
func TestHandleH264_STAPAZeroSizeDoesNotPanic(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		// STAP-A NAL header (0x78 = NRI=3 type=24) followed by a single size
		// field that claims 0 bytes. Total payload = 3 bytes, so packet=nal[1:]
		// has length 2 and packet[2] is OOB.
		{name: "size_zero_exact_two", payload: []byte{0x78, 0x00, 0x00}},

		// Same but with one trailing byte. size+2 = 2 is still <= 3, so
		// without the size==0 check the loop would proceed.
		{name: "size_zero_with_trailing", payload: []byte{0x78, 0x00, 0x00, 0xAB}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestH264Client()
			pkt := makeInterleavedRTPPacket(8, true, 1, 1000, 0xBEEF, tc.payload)
			if callDemuxerRecovering(c, pkt) {
				t.Fatalf("STAP-A with size=0 panicked the demuxer")
			}
		})
	}
}

// TestHandleH264_STAPAOversizedDoesNotPanic re-confirms the existing bounds
// check still works for the size-too-big case after we tightened the check.
func TestHandleH264_STAPAOversizedDoesNotPanic(t *testing.T) {
	c := newTestH264Client()
	// STAP-A header + size=100 but only 4 bytes of NAL data follow.
	pkt := makeInterleavedRTPPacket(8, true, 1, 1000, 0xBEEF,
		[]byte{0x78, 0x00, 0x64, 0x65, 0x01, 0x02, 0x03})
	if callDemuxerRecovering(c, pkt) {
		t.Fatalf("STAP-A with oversized size panicked the demuxer")
	}
}

// ----------------------------------------------------------------------------
// Application/metadata path: ONVIF MetadataStream reassembly.
//
// Per ONVIF Streaming Specification §5.2 the RTP marker bit terminates one XML
// document. Fragments sharing a timestamp are concatenated up to and including
// the packet whose marker is set.
// ----------------------------------------------------------------------------

func TestHandleMetadata_SinglePacketDoc(t *testing.T) {
	c := newTestMetadataClient(8)
	payload := []byte(`<?xml version="1.0"?><tt:MetadataStream xmlns:tt="x"/>`)
	pkt := makeMetadataPacket(4, true, 1, 1000, payload)

	if pkts, got := c.RTPDemuxer(&pkt); got || pkts != nil {
		t.Fatalf("RTPDemuxer should return (nil,false) for metadata, got pkts=%v got=%v", pkts, got)
	}

	select {
	case doc := <-c.OutgoingMetadataQueue:
		if !bytes.Equal(doc, payload) {
			t.Errorf("doc mismatch\n got: %q\nwant: %q", doc, payload)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected a document on OutgoingMetadataQueue, none arrived")
	}
}

func TestHandleMetadata_FragmentedDocReassembles(t *testing.T) {
	c := newTestMetadataClient(8)
	parts := [][]byte{
		[]byte(`<?xml version="1.0"?><tt:Met`),
		[]byte(`adataStream xmlns:tt="x">`),
		[]byte(`<tt:PTZ/></tt:MetadataStream>`),
	}
	want := bytes.Join(parts, nil)

	for i, p := range parts {
		marker := i == len(parts)-1
		pkt := makeMetadataPacket(4, marker, uint16(i+1), 2000, p)
		c.RTPDemuxer(&pkt)
	}

	select {
	case doc := <-c.OutgoingMetadataQueue:
		if !bytes.Equal(doc, want) {
			t.Errorf("reassembled mismatch\n got: %q\nwant: %q", doc, want)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected a reassembled document, none arrived")
	}

	// No spurious second doc.
	select {
	case extra := <-c.OutgoingMetadataQueue:
		t.Fatalf("unexpected extra doc: %q", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

// TestHandleMetadata_TimestampChangeResetsBuffer: a partial fragment whose
// final marker packet is lost must not leak into the next document when a new
// timestamp arrives.
func TestHandleMetadata_TimestampChangeResetsBuffer(t *testing.T) {
	c := newTestMetadataClient(8)

	lost := []byte(`<tt:LOST_DOC>this should never appear in any emitted doc</tt:LOST_DOC>`)
	pktA := makeMetadataPacket(4, false, 1, 3000, lost)
	c.RTPDemuxer(&pktA)

	clean := []byte(`<tt:MetadataStream><tt:PTZ/></tt:MetadataStream>`)
	pktB := makeMetadataPacket(4, true, 2, 4000, clean)
	c.RTPDemuxer(&pktB)

	select {
	case doc := <-c.OutgoingMetadataQueue:
		if bytes.Contains(doc, []byte("LOST_DOC")) {
			t.Errorf("emitted doc carried stale bytes from the lost previous document: %q", doc)
		}
		if !bytes.Equal(doc, clean) {
			t.Errorf("doc mismatch\n got: %q\nwant: %q", doc, clean)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected the second document, none arrived")
	}
}

func TestHandleMetadata_MultipleDocsSequence(t *testing.T) {
	c := newTestMetadataClient(8)
	docs := [][]byte{
		[]byte(`<a>1</a>`),
		[]byte(`<b>2</b>`),
		[]byte(`<c>3</c>`),
	}
	for i, d := range docs {
		pkt := makeMetadataPacket(4, true, uint16(i+1), uint32(5000+i), d)
		c.RTPDemuxer(&pkt)
	}
	for i, want := range docs {
		select {
		case got := <-c.OutgoingMetadataQueue:
			if !bytes.Equal(got, want) {
				t.Errorf("doc #%d mismatch\n got: %q\nwant: %q", i, got, want)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("expected doc #%d, none arrived", i)
		}
	}
}

// TestHandleMetadata_NonBlockingDropsOldest: when the consumer never reads,
// the producer must not block. The drop-oldest policy keeps the RTSP reader
// goroutine moving even when downstream is overwhelmed.
func TestHandleMetadata_NonBlockingDropsOldest(t *testing.T) {
	c := newTestMetadataClient(2)
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			payload := []byte("doc-" + string(rune('A'+i)))
			pkt := makeMetadataPacket(4, true, uint16(i+1), uint32(6000+i), payload)
			c.RTPDemuxer(&pkt)
		}
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("producer blocked when consumer was not reading; non-blocking drop policy broken")
	}

	seenNewer := false
	for {
		select {
		case doc := <-c.OutgoingMetadataQueue:
			if strings.HasPrefix(string(doc), "doc-") && len(doc) > 4 && doc[4] >= 'D' {
				seenNewer = true
			}
		case <-time.After(50 * time.Millisecond):
			if !seenNewer {
				t.Error("queue contained only oldest docs; expected newer docs to displace older ones")
			}
			return
		}
	}
}

// TestHandleMetadata_IgnoresUnknownChannels: a packet on a channel that is not
// video, audio, or metadata must be silently dropped.
func TestHandleMetadata_IgnoresUnknownChannels(t *testing.T) {
	c := newTestMetadataClient(8)
	pkt := makeMetadataPacket(6, true, 1, 7000, []byte("payload"))
	if pkts, got := c.RTPDemuxer(&pkt); got || pkts != nil {
		t.Errorf("expected (nil,false) for unknown channel, got %v %v", pkts, got)
	}
	select {
	case doc := <-c.OutgoingMetadataQueue:
		t.Errorf("expected no doc for unknown channel, got %q", doc)
	case <-time.After(20 * time.Millisecond):
	}
}
