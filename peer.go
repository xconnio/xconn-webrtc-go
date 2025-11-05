package xconnwebrtc

import (
	"net"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/xconn-go"
)

type WebRTCPeer struct {
	channel *webrtc.DataChannel

	messageChan chan []byte
	assembler   *WebRTCMessageAssembler
}

func NewWebRTCPeer(channel *webrtc.DataChannel) xconn.Peer {
	messageChan := make(chan []byte, 1)

	assembler := NewWebRTCMessageAssembler(MtuSize)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		toSend := assembler.Feed(msg.Data)

		if toSend != nil {
			messageChan <- toSend
		}
	})

	return &WebRTCPeer{
		channel:     channel,
		messageChan: messageChan,
		assembler:   assembler,
	}
}

func (w *WebRTCPeer) Type() xconn.TransportType {
	return xconn.TransportNone
}

func (w *WebRTCPeer) NetConn() net.Conn {
	return nil
}

func (w *WebRTCPeer) Read() ([]byte, error) {
	return <-w.messageChan, nil
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
	return w.channel.Close()
}
