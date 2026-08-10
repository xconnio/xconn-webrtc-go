package xconnwebrtc

import (
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/wampproto-go/transports"
	"github.com/xconnio/xconn-go"
)

// serializersByRawSocketID resolves a serializers.Serializer from the
// RawSocket serializer id a handshake negotiates, mirroring xconn-go's
// internal RawSocket serializer registry.
var serializersByRawSocketID = map[transports.Serializer]serializers.Serializer{ //nolint:gochecknoglobals
	transports.SerializerJson:    &serializers.JSONSerializer{},
	transports.SerializerMsgpack: &serializers.MsgPackSerializer{},
	transports.SerializerCbor:    &serializers.CBORSerializer{},
}

func buildHandshake(serializer transports.Serializer) ([]byte, error) {
	return transports.SendHandshake(transports.NewHandshake(serializer, transports.DefaultMaxMsgSize))
}

// wampHandshake reports whether data is a 4-byte WAMP RawSocket-style
// handshake message.
func wampHandshake(data []byte) (transports.Serializer, bool) {
	if len(data) != 4 || data[0] != transports.MAGIC {
		return 0, false
	}

	hs, err := transports.ReceiveHandshake(data)
	if err != nil {
		return 0, false
	}

	return hs.Serializer(), true
}

// sendClientHandshake performs the client side of the magic-byte handshake
// on an already-open channel: send our handshake, then wait for the
// server's response before any WAMP traffic flows.
func sendClientHandshake(channel *webrtc.DataChannel, spec xconn.SerializerSpec, timeout time.Duration) error {
	reqBytes, err := buildHandshake(transports.Serializer(spec.SerializerID()))
	if err != nil {
		return fmt.Errorf("failed to build handshake: %w", err)
	}

	respCh := make(chan []byte, 1)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case respCh <- msg.Data:
		default:
		}
	})

	if err = channel.Send(reqBytes); err != nil {
		return fmt.Errorf("failed to send handshake: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-respCh:
		if _, err = transports.ReceiveHandshake(resp); err != nil {
			return fmt.Errorf("failed to parse handshake response: %w", err)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out waiting for handshake response")
	}
}
