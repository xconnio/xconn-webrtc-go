package xconnwebrtc_test

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/xconn-go"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

// connectedPeers returns the two ends of a data channel between two peer
// connections in this process, as WebRTCPeers.
func connectedPeers(t *testing.T) (xconn.Peer, xconn.Peer) {
	t.Helper()

	offerer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = offerer.Close() })
	answerer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = answerer.Close() })

	ordered := true
	offererChannel, err := offerer.CreateDataChannel("data", &webrtc.DataChannelInit{Ordered: &ordered})
	require.NoError(t, err)

	opened := make(chan struct{})
	var once sync.Once
	offererChannel.OnOpen(func() { once.Do(func() { close(opened) }) })
	answererChannel := make(chan *webrtc.DataChannel, 1)
	answerer.OnDataChannel(func(channel *webrtc.DataChannel) { answererChannel <- channel })

	offer, err := offerer.CreateOffer(nil)
	require.NoError(t, err)
	offerGathered := webrtc.GatheringCompletePromise(offerer)
	require.NoError(t, offerer.SetLocalDescription(offer))
	<-offerGathered
	require.NoError(t, answerer.SetRemoteDescription(*offerer.LocalDescription()))

	answer, err := answerer.CreateAnswer(nil)
	require.NoError(t, err)
	answerGathered := webrtc.GatheringCompletePromise(answerer)
	require.NoError(t, answerer.SetLocalDescription(answer))
	<-answerGathered
	require.NoError(t, offerer.SetRemoteDescription(*answerer.LocalDescription()))

	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("data channel didn't open")
	}
	var remote *webrtc.DataChannel
	select {
	case remote = <-answererChannel:
	case <-time.After(10 * time.Second):
		t.Fatal("answerer didn't get the data channel")
	}
	// The receiving peer must be listening before anything is sent.
	return xconnwebrtc.NewWebRTCPeer(offererChannel), xconnwebrtc.NewWebRTCPeer(remote)
}

// TestWebRTCPeerConcurrentWrites writes messages spanning many chunks from
// several goroutines at once; each must arrive whole, not interleaved with
// another's chunks.
func TestWebRTCPeerConcurrentWrites(t *testing.T) {
	sender, receiver := connectedPeers(t)

	const writers, perWriter = 8, 5
	// Every message is filled with its own 4-byte id, so a garbled one shows.
	message := func(id uint32) []byte {
		size := 4 // a small message for every other id: it fits one chunk
		if id%2 == 0 {
			size = 100 * 1024 // spans several chunks
		}
		msg := make([]byte, size)
		for i := 0; i+4 <= len(msg); i += 4 {
			binary.BigEndian.PutUint32(msg[i:], id)
		}
		return msg
	}

	var wg sync.WaitGroup
	writeErrs := make(chan error, writers*perWriter)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				writeErrs <- sender.Write(message(uint32(w*perWriter + i))) // nolint: gosec
			}
		}()
	}

	received := make(map[uint32]bool)
	for range writers * perWriter {
		msg, err := receiver.Read()
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(msg), 4)
		id := binary.BigEndian.Uint32(msg)
		require.True(t, bytes.Equal(message(id), msg), "message %d arrived garbled (%d bytes)", id, len(msg))
		require.False(t, received[id], "message %d arrived twice", id)
		received[id] = true
	}
	wg.Wait()
	close(writeErrs)
	for err := range writeErrs {
		require.NoError(t, err)
	}
}
