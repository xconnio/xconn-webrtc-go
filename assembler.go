package xconnwebrtc

import (
	"bytes"
	"iter"
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
	out := make(chan []byte, m.chunkCount(message))
	for chunk := range m.chunks(message) {
		out <- chunk
	}
	close(out)
	return out
}

// chunkCount is how many chunks message is sent in. An empty message is still
// one (empty, final) chunk, so the receiver gets it.
func (m *WebRTCMessageAssembler) chunkCount(message []byte) int {
	chunkSize := m.mtu - 1
	return max(1, (len(message)+chunkSize-1)/chunkSize)
}

// chunks yields message's chunks, as ChunkMessage describes, each made only
// when it's asked for so a large message isn't copied whole at once.
func (m *WebRTCMessageAssembler) chunks(message []byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		chunkSize := m.mtu - 1
		total := m.chunkCount(message)
		for i := range total {
			start := i * chunkSize
			end := min(start+chunkSize, len(message))

			var isFinal byte
			if i == total-1 {
				isFinal = 1
			}
			if !yield(append([]byte{isFinal}, message[start:end]...)) {
				return
			}
		}
	}
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
