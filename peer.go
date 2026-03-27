package xconnwebrtc

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/xconn-go"
)

type WebRTCPeer struct {
	channel    *webrtc.DataChannel
	connection *webrtc.PeerConnection

	messageChan chan []byte
	assembler   *WebRTCMessageAssembler

	done      chan struct{}
	readErr   error
	closeOnce sync.Once

	sync.Mutex
	disconnectTimer    *time.Timer
	disconnectDeadline time.Duration
}

func NewWebRTCPeer(channel *webrtc.DataChannel, connection *webrtc.PeerConnection) xconn.Peer {
	messageChan := make(chan []byte, 1)

	peer := &WebRTCPeer{
		channel:            channel,
		connection:         connection,
		messageChan:        messageChan,
		assembler:          NewWebRTCMessageAssembler(MtuSize),
		done:               make(chan struct{}),
		readErr:            io.EOF,
		disconnectDeadline: 1 * time.Second,
	}

	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		toSend := peer.assembler.Feed(msg.Data)
		if toSend == nil {
			return
		}

		select {
		case peer.messageChan <- toSend:
		case <-peer.done:
		}
	})

	channel.OnError(func(err error) {
		peer.closeWithError(err)
	})

	channel.OnClose(func() {
		peer.closeWithError(io.EOF)
	})

	if connection != nil {
		connection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			peer.handleConnectionStateChange(state)
		})
	}

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
		w.Lock()
		err := w.readErr
		w.Unlock()
		return nil, err
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
	w.closeWithError(io.EOF)
	return w.channel.Close()
}

func (w *WebRTCPeer) closeWithError(err error) {
	if err == nil {
		err = io.EOF
	}

	w.closeOnce.Do(func() {
		w.Lock()
		if w.disconnectTimer != nil {
			w.disconnectTimer.Stop()
			w.disconnectTimer = nil
		}
		w.readErr = err
		w.Unlock()

		close(w.done)
	})
}

func (w *WebRTCPeer) handleConnectionStateChange(state webrtc.PeerConnectionState) {
	switch state {
	case webrtc.PeerConnectionStateConnected:
		w.stopDisconnectTimer()
	case webrtc.PeerConnectionStateDisconnected:
		w.startDisconnectTimer()
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		w.closeWithError(io.EOF)
	default:
	}
}

func (w *WebRTCPeer) startDisconnectTimer() {
	w.Lock()
	defer w.Unlock()

	if w.disconnectTimer != nil {
		return
	}

	w.disconnectTimer = time.AfterFunc(w.disconnectDeadline, func() {
		if w.connection != nil && w.connection.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return
		}
		w.closeWithError(io.EOF)
	})
}

func (w *WebRTCPeer) stopDisconnectTimer() {
	w.Lock()
	defer w.Unlock()

	if w.disconnectTimer == nil {
		return
	}

	w.disconnectTimer.Stop()
	w.disconnectTimer = nil
}
