package xconnwebrtc

import (
	"io"
	"net"
	"sync"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/xconn-go"
)

type WebRTCPeer struct {
	channel *webrtc.DataChannel

	messageChan chan []byte
	assembler   *WebRTCMessageAssembler

	done      chan struct{}
	closeOnce sync.Once
}

func NewWebRTCPeer(channel *webrtc.DataChannel) xconn.Peer {
	messageChan := make(chan []byte, 1)

	assembler := NewWebRTCMessageAssembler(MtuSize)

	peer := &WebRTCPeer{
		channel:     channel,
		messageChan: messageChan,
		assembler:   assembler,
		done:        make(chan struct{}),
	}
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		toSend := assembler.Feed(msg.Data)
		if toSend == nil {
			return
		}

		select {
		case peer.messageChan <- toSend:
		case <-peer.done:
		}
	})

	return peer
}

func (w *WebRTCPeer) Type() xconn.TransportType {
	return xconn.TransportNone
}

func (w *WebRTCPeer) NetConn() net.Conn {
	return nil
}

func (w *WebRTCPeer) Read() ([]byte, error) {
	select {
	case msg := <-w.messageChan:
		return msg, nil
	case <-w.done:
		return nil, io.EOF
	}
}

func (w *WebRTCPeer) Write(bytes []byte) error {
	for chunk := range w.assembler.ChunkMessage(bytes) {
		if err := w.channel.Send(chunk); err != nil {
			return err
		}
	}

	return nil
}

func (w *WebRTCPeer) TryWrite(bytes []byte) (bool, error) {
	if err := w.Write(bytes); err != nil {
		return false, err
	}

	return true, nil
}

func (w *WebRTCPeer) Close() error {
	w.closeOnce.Do(func() {
		close(w.done)
	})
	return w.channel.Close()
}
