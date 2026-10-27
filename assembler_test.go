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

	t.Run("ChunkMessage of exact multiples of the chunk size", func(t *testing.T) {
		chunkSize := xconnwebrtc.MtuSize - 1
		for _, size := range []int{chunkSize, 2 * chunkSize} {
			message := make([]byte, size)
			assembler := xconnwebrtc.NewWebRTCMessageAssembler(xconnwebrtc.MtuSize)

			var chunks [][]byte
			for chunk := range assembler.ChunkMessage(message) {
				chunks = append(chunks, chunk)
			}
			require.Len(t, chunks, size/chunkSize)
			for i, chunk := range chunks {
				require.Equal(t, i == len(chunks)-1, chunk[0] == 1, "only the last chunk is final")
			}
		}
	})

	t.Run("ChunkMessage holds all chunks for a reader that stops early", func(t *testing.T) {
		assembler := xconnwebrtc.NewWebRTCMessageAssembler(xconnwebrtc.MtuSize)
		chunks := assembler.ChunkMessage(make([]byte, 40*1024))
		require.Equal(t, 3, len(chunks), "nothing is left waiting to send the rest")
		<-chunks
	})

	t.Run("an empty message arrives", func(t *testing.T) {
		assembler := xconnwebrtc.NewWebRTCMessageAssembler(xconnwebrtc.MtuSize)
		var received []byte
		for chunk := range assembler.ChunkMessage(nil) {
			received = assembler.Feed(chunk)
		}
		require.NotNil(t, received)
		require.Empty(t, received)
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
