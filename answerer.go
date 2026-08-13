package xconnwebrtc

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
)

type Answerer struct {
	connection *webrtc.PeerConnection

	// onWAMPDataChannel fires for every data channel identified as WAMP,
	// first channel or not; each one becomes an independent WAMP session. A
	// channel is identified as WAMP either by its first message being a
	// magic-byte handshake, or — for the connection's first channel only,
	// matching pre-handshake clients' "first channel is WAMP by convention, no handshake sent"
	//behavior — by its protocol string naming a WAMP subprotocol.
	// onDataChannel fires for everything else, with the first message handed
	// back when one was already consumed to classify it.
	onWAMPDataChannel func(channel *webrtc.DataChannel, serializer serializers.Serializer)
	onDataChannel     func(channel *webrtc.DataChannel, firstMessage []byte)
	onIceCandidate    func(candidate *webrtc.ICECandidate)
	cachedCandidates  []webrtc.ICECandidateInit

	sync.Mutex
}

func NewAnswerer() *Answerer {
	return &Answerer{}
}

// OnWAMPDataChannel registers a callback fired for every data channel whose
// first message identifies it as WAMP, first channel or not — each one is an
// independent WAMP session sharing this connection. See OnDataChannel for the
// synchronous-callback caveat, which applies here too.
func (a *Answerer) OnWAMPDataChannel(callback func(channel *webrtc.DataChannel, serializer serializers.Serializer)) {
	a.Lock()
	defer a.Unlock()

	a.onWAMPDataChannel = callback
}

// OnDataChannel registers a callback fired for every data channel whose first
// message isn't a WAMP handshake. firstMessage is that already-consumed first message,
// it was read to determine the channel wasn't a WAMP session and will not be redelivered via
// channel.OnMessage, so the callback must handle it directly.
func (a *Answerer) OnDataChannel(callback func(channel *webrtc.DataChannel, firstMessage []byte)) {
	a.Lock()
	defer a.Unlock()

	a.onDataChannel = callback
}

func (a *Answerer) Answer(answerConfig *AnswerConfig, offer Offer, trickleAfter time.Duration) (*Answer, error) {
	start := time.Now()
	end := start.Add(trickleAfter)

	connection, err := NewFilteredPeerConnection(answerConfig.ICEServers)
	if err != nil {
		return nil, err
	}

	a.Lock()
	a.connection = connection
	a.Unlock()

	connection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Debugf("answerer ICE connection state: %s (+%s)", state, time.Since(start))
	})

	if err = connection.SetRemoteDescription(offer.Description); err != nil {
		return nil, err
	}

	done := make(chan struct{})
	var trickle = false
	var initialCandidates []webrtc.ICECandidateInit
	connection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			log.Debugf("answerer ICE gathering complete (+%s)", time.Since(start))
			return
		}

		if trickle || time.Now().After(end) {
			a.Lock()
			cb := a.onIceCandidate
			a.Unlock()
			if cb != nil {
				go cb(candidate)
			}
		} else {
			initialCandidates = append(initialCandidates, candidate.ToJSON())
			// host candidate gathering is done, any further candidates should
			// be signaled with Trickle ICE.
			if candidate.Typ != webrtc.ICECandidateTypeHost {
				trickle = true
				select {
				case done <- struct{}{}:
				default:
				}
			}
		}
	})

	// legacySerializers recognizes pre-handshake clients: they identify their
	// single WAMP channel by setting the DataChannel protocol to a WAMP
	// subprotocol string and start writing WAMP messages immediately, with no
	// handshake at all. Only ever consulted for the connection's first
	// channel (see firstChannel below) — that matches those clients exactly,
	// since they never open more than one channel, and it means a later raw
	// channel can never be misclassified as WAMP just because its protocol
	// string happens to collide with one of these.
	legacySerializers := xconn.SerializersByWSSubProtocol()
	var firstChannel atomic.Bool

	// A channel's first message decides what it is: a WAMP RawSocket-style
	// magic-byte handshake makes it a new WAMP session; anything else is handed
	// to onDataChannel, first message included. The handler below only ever fires
	// once per channel: on the WAMP path, NewWebRTCPeer replaces it;
	// on the raw path, onDataChannel's contract requires the caller to
	// replace it too.
	connection.OnDataChannel(func(d *webrtc.DataChannel) {
		if firstChannel.CompareAndSwap(false, true) {
			if serializer, ok := legacySerializers[d.Protocol()]; ok {
				a.Lock()
				cb := a.onWAMPDataChannel
				a.Unlock()
				if cb != nil {
					cb(d, serializer)
				}
				return
			}
		}

		detected := false
		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			if detected {
				return
			}
			detected = true

			serializerID, ok := wampHandshake(msg.Data)
			if !ok {
				a.Lock()
				cb := a.onDataChannel
				a.Unlock()
				if cb != nil {
					cb(d, msg.Data)
				}
				return
			}

			serializer, ok := serializersByRawSocketID[serializerID]
			if !ok {
				log.Debugf("answerer: unsupported serializer %d in handshake on channel %q", serializerID, d.Label())
				return
			}

			respBytes, err := buildHandshake(serializerID)
			if err != nil {
				log.Debugf("answerer: failed to build handshake response: %v", err)
				return
			}
			if err = d.Send(respBytes); err != nil {
				log.Debugf("answerer: failed to send handshake response: %v", err)
				return
			}

			a.Lock()
			cb := a.onWAMPDataChannel
			a.Unlock()
			if cb != nil {
				cb(d, serializer)
			}
		})
	})

	answer, err := connection.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}

	if err = connection.SetLocalDescription(answer); err != nil {
		return nil, err
	}

	for _, candidate := range offer.Candidates {
		if err = connection.AddICECandidate(candidate); err != nil {
			log.Debugf("failed to add offer ICE candidate: %v", err)
		}
	}

	a.Lock()
	for _, candidate := range a.cachedCandidates {
		if err = connection.AddICECandidate(candidate); err != nil {
			log.Debugf("failed to add cached ICE candidate: %v", err)
		}
	}
	a.cachedCandidates = nil
	a.Unlock()

	select {
	case <-done:
	case <-time.After(time.Until(end)):
	}

	return &Answer{
		Candidates:  initialCandidates,
		Description: answer,
	}, nil
}

func (a *Answerer) OnIceCandidate(callback func(candidate *webrtc.ICECandidate)) {
	a.Lock()
	defer a.Unlock()

	a.onIceCandidate = callback
}

func (a *Answerer) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	a.Lock()
	defer a.Unlock()

	if a.connection == nil {
		a.cachedCandidates = append(a.cachedCandidates, candidate)
		return nil
	} else {
		return a.connection.AddICECandidate(candidate)
	}
}

// Connection returns the underlying PeerConnection, or nil if not yet established.
func (a *Answerer) Connection() *webrtc.PeerConnection {
	a.Lock()
	defer a.Unlock()
	return a.connection
}
