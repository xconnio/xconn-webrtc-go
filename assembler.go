package xconnwebrtc

import (
	"bytes"
	"sync"
)

const MtuSize = 16 * 1024

type WebRTCMessageAssembler struct {
	buffer *bytes.Buffer
	mtu    int

	sync.Mutex
}

func NewWebRTCMessageAssembler(mtu int) *WebRTCMessageAssembler {
	return &WebRTCMessageAssembler{
		buffer: bytes.NewBuffer(nil),
		mtu:    mtu,
	}
}

func (m *WebRTCMessageAssembler) ChunkMessage(message []byte) chan []byte {
	m.Lock()
	defer m.Unlock()

	chunkSize := m.mtu - 1
	totalChunks := (len(message) + chunkSize - 1) / chunkSize

	chunks := make(chan []byte)

	go func() {
		for i := 0; i < totalChunks; i++ {
			start := i * chunkSize
			end := start + chunkSize
			if i == totalChunks-1 {
				end = len(message)
			}
			chunk := message[start:end]

			var isFinal byte = 0
			if i == totalChunks-1 {
				isFinal = 1
			}

			chunks <- append([]byte{isFinal}, chunk...)
		}
		close(chunks)
	}()

	return chunks
}

func (m *WebRTCMessageAssembler) Feed(data []byte) []byte {
	m.Lock()
	defer m.Unlock()

	if len(data) == 0 {
		return nil
	}

	m.buffer.Write(data[1:])
	isFinal := data[0]

	if isFinal == 1 {
		out := make([]byte, m.buffer.Len())
		copy(out, m.buffer.Bytes())
		m.buffer.Reset()
		return out
	}

	return nil
}
