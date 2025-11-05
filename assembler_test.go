package xconnwebrtc_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/xconn-webrtc-go"
)

func TestWebRTCMessageAssembler(t *testing.T) {
	t.Run("ChunkMessage", func(t *testing.T) {
		message := make([]byte, 40*1024)
		for i := range message {
			message[i] = byte(i % 256)
		}

		assembler := xconnwebrtc.NewWebRTCMessageAssembler(xconnwebrtc.MtuSize)
		chunks := assembler.ChunkMessage(message)

		var reconstructedMessage []byte
		var chunkCount int

		for chunk := range chunks {
			chunkCount++
			require.LessOrEqual(t, len(chunk), 16*1024)

			// Final chunk
			if chunk[0] == 1 {
				require.Equal(t, 3, chunkCount)
			}
			reconstructedMessage = append(reconstructedMessage, chunk[1:]...)
		}

		require.Equal(t, message, reconstructedMessage)
	})

	t.Run("Feed", func(t *testing.T) {
		message := []byte("Hello, World!")
		assembler := xconnwebrtc.NewWebRTCMessageAssembler(xconnwebrtc.MtuSize)

		chunks := assembler.ChunkMessage(message)
		var finalMessage []byte

		for chunk := range chunks {
			finalMessage = assembler.Feed(chunk)
			if chunk[0] != 1 {
				require.Nil(t, finalMessage)
			}
		}

		require.Equal(t, message, finalMessage)
	})
}
