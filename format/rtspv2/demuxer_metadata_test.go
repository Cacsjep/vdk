package rtspv2

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// makeMetadataPacket builds a bytes-on-wire interleaved RTP packet for the
// metadata channel that vdk reads, mirroring exactly what conn.Read would deliver:
// 4 bytes interleaved header + 12 bytes RTP header + payload.
//
//	[0]: 0x24 (RTP-over-TCP magic, $)
//	[1]: channel id  (= client.metadataID)
//	[2:4]: length BE
//	[4]: RTP v=2, no padding, no extension, CC=0  => 0x80
//	[5]: marker bit + payload type (PT does not matter for dispatch, just channel)
//	[6:8]: sequence number BE
//	[8:12]: timestamp BE
//	[12:16]: SSRC BE
//	[16:]: payload
func makeMetadataPacket(channel byte, marker bool, seq uint16, ts uint32, payload []byte) []byte {
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
	binary.BigEndian.PutUint32(buf[12:16], 0xDEADBEEF)
	copy(buf[16:], payload)
	return buf
}

// newTestMetadataClient returns a minimal RTSPClient wired up so that
// RTPDemuxer will dispatch to handleMetadata on channel 4. No network.
func newTestMetadataClient(queueSize int) *RTSPClient {
	return &RTSPClient{
		videoID:               -1,
		audioID:               -2,
		metadataID:            4,
		OutgoingMetadataQueue: make(chan []byte, queueSize),
		BufferRtpPacket:       bytes.NewBuffer(nil),
	}
}

// TestMetadataSinglePacketDoc: one packet with marker=1 emits one document
// equal to its payload.
func TestMetadataSinglePacketDoc(t *testing.T) {
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

// TestMetadataFragmentedDoc: three packets sharing one timestamp, only the
// last carries marker=1. Reassembled payload must be the concatenation.
func TestMetadataFragmentedDoc(t *testing.T) {
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

// TestMetadataTimestampChangeResetsBuffer: a partial fragment whose final
// marker packet is lost must not leak into the next document when a new
// timestamp arrives.
func TestMetadataTimestampChangeResetsBuffer(t *testing.T) {
	c := newTestMetadataClient(8)

	// Document A: send a fragment without ever sending its marker packet.
	lost := []byte(`<tt:LOST_DOC>this should never appear in any emitted doc</tt:LOST_DOC>`)
	pktA := makeMetadataPacket(4, false, 1, 3000, lost)
	c.RTPDemuxer(&pktA)

	// Document B: single-packet doc at a new timestamp with marker=1.
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

// TestMetadataMultipleDocsSequence: several complete docs in a row each emit
// once, preserving order.
func TestMetadataMultipleDocsSequence(t *testing.T) {
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

// TestMetadataNonBlockingDropsOldest: when the consumer never reads, the
// producer must not block. Confirmed by sending more docs than the queue
// capacity within a tight window and observing that newer docs displace
// older ones rather than the call hanging.
func TestMetadataNonBlockingDropsOldest(t *testing.T) {
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

	// Drain whatever's queued and confirm at least one of the newer docs survived.
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

// TestMetadataIgnoredWhenChannelDoesNotMatch: a packet on an unrelated channel
// (not video, audio, or metadata) must not produce a document.
func TestMetadataIgnoredWhenChannelDoesNotMatch(t *testing.T) {
	c := newTestMetadataClient(8)
	// Channel 6 is not registered (metadataID=4, video=-1, audio=-2).
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
