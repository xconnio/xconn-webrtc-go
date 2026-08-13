package xconnwebrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
)

type WebRTCProvider struct {
	answerers     map[string]*Answerer
	onNewAnswerer func(sessionID string, answerer *Answerer)
	// onDataChannel receives every data channel that isn't a WAMP session.
	onDataChannel func(sessionID string, channel *webrtc.DataChannel, firstMessage []byte)

	iceServers []webrtc.ICEServer

	sync.Mutex
}

func NewWebRTCHandler() *WebRTCProvider {
	return &WebRTCProvider{
		answerers: make(map[string]*Answerer),
	}
}

func (r *WebRTCProvider) UpdateICEServers(servers []ICEServer) {
	r.Lock()
	defer r.Unlock()

	r.iceServers = cloneICEServers(servers)
}

func (r *WebRTCProvider) OnAnswerer(callback func(sessionID string, answerer *Answerer)) {
	r.Lock()
	defer r.Unlock()

	r.onNewAnswerer = callback
}

// OnDataChannel registers a callback that fires for every data channel opened
// by a client that isn't a WAMP session.
func (r *WebRTCProvider) OnDataChannel(callback func(sessionID string,
	channel *webrtc.DataChannel, firstMessage []byte)) {
	r.Lock()
	defer r.Unlock()

	r.onDataChannel = callback
}

func (r *WebRTCProvider) ensureAnswerer(sessionID string) *Answerer {
	r.Lock()
	defer r.Unlock()

	answerer, exists := r.answerers[sessionID]
	if !exists {
		answerer = NewAnswerer()
		r.answerers[sessionID] = answerer
		if r.onNewAnswerer != nil {
			r.onNewAnswerer(sessionID, answerer)
		}
	}

	return answerer
}

func (r *WebRTCProvider) removeAnswerer(sessionID string, answerer *Answerer) {
	r.Lock()
	current, exists := r.answerers[sessionID]
	if !exists || current != answerer {
		r.Unlock()
		return
	}
	delete(r.answerers, sessionID)
	r.Unlock()

	if answerer.connection != nil {
		if err := answerer.connection.Close(); err != nil {
			log.Debugf("failed to close peer connection for %s: %v", sessionID, err)
		}
	}
}

func (r *WebRTCProvider) addIceCandidate(requestID string, candidate webrtc.ICECandidateInit) error {
	answerer := r.ensureAnswerer(requestID)
	return answerer.AddICECandidate(candidate)
}

func (r *WebRTCProvider) handleOffer(requestID string, offer Offer, answerConfig *AnswerConfig) (*Answer, error) {
	answerer := r.ensureAnswerer(requestID)
	answer, err := answerer.Answer(answerConfig, offer, 100*time.Millisecond)
	if err != nil {
		r.removeAnswerer(requestID, answerer)
		return nil, err
	}

	if answerer.connection != nil {
		answerer.connection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			switch state {
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				r.removeAnswerer(requestID, answerer)
			case webrtc.PeerConnectionStateDisconnected:
				log.Debugf("peer connection disconnected for %s; keeping answerer alive for recovery", requestID)
			default:
			}
		})
	}

	return answer, nil
}

func (r *WebRTCProvider) Setup(config *ProviderConfig) error {
	if err := config.validate(); err != nil {
		return fmt.Errorf("invalid provider config: %w", err)
	}
	r.iceServers = cloneICEServers(config.ICEServers)
	registerResp := config.Session.Register(config.ProcedureHandleOffer, r.offerFunc).Do()
	if registerResp.Err != nil {
		return fmt.Errorf("failed to register webrtc offer: %w", registerResp.Err)
	}

	subscribeResp := config.Session.Subscribe(config.TopicHandleRemoteCandidates, r.onRemoteCandidate).Do()
	if subscribeResp.Err != nil {
		return fmt.Errorf("failed to subscribe to webrtc candidates events: %w", subscribeResp.Err)
	}

	r.OnAnswerer(func(sessionID string, answerer *Answerer) {
		answerer.OnIceCandidate(func(candidate *webrtc.ICECandidate) {
			answerData, err := json.Marshal(candidate.ToJSON())
			if err != nil {
				log.Debugf("failed to marshal answer: %v", err)
				return
			}

			args := []any{sessionID, string(answerData)}
			publishResp := config.Session.Publish(config.TopicPublishLocalCandidate).Args(args...).Do()
			if publishResp.Err != nil {
				log.Debugf("failed to publish answer: %v", publishResp.Err)
			}
		})

		answerer.OnDataChannel(func(channel *webrtc.DataChannel, firstMessage []byte) {
			r.Lock()
			cb := r.onDataChannel
			r.Unlock()
			if cb != nil {
				cb(sessionID, channel, firstMessage)
			}
		})

		var sessionEstablished atomic.Bool
		answerer.OnWAMPDataChannel(func(channel *webrtc.DataChannel, serializer serializers.Serializer) {
			sessionEstablished.Store(true)

			// NewWebRTCPeer must run synchronously here, before this callback
			// returns: it registers channel.OnMessage, and pion won't start
			// delivering messages on the channel until this callback returns
			// (see OnDataChannel's doc comment). Deferring it into the
			// goroutine below would race the client's HELLO against handler
			// registration and could silently drop it.
			rtcPeer := NewWebRTCPeer(channel)
			go func() {
				if err := r.handleWAMPClient(sessionID, channel, rtcPeer, serializer, config); err != nil {
					log.Debugf("failed to handle WAMP data channel for session %s: %v", sessionID, err)
				}
			}()
		})

		go func() {
			<-time.After(20 * time.Second)
			if !sessionEstablished.Load() {
				log.Debugln("webrtc connection didn't establish after 20 seconds")
				r.removeAnswerer(sessionID, answerer)
			}
		}()
	})

	return nil
}

// handleWAMPClient runs one WAMP session on channel: RawSocket-equivalent
// HELLO/WELCOME handshake, router attach, message loop. A connection can host
// several concurrent sessions (see OnWAMPDataChannel), so a failure here must
// only tear down this session, not the whole PeerConnection/Answerer.
func (r *WebRTCProvider) handleWAMPClient(sessionID string, channel *webrtc.DataChannel,
	rtcPeer xconn.Peer, serializer serializers.Serializer, config *ProviderConfig) error {

	hello, err := xconn.ReadHello(rtcPeer, serializer)
	if err != nil {
		return err
	}

	base, err := xconn.Accept(rtcPeer, hello, serializer, config.Authenticator)
	if err != nil {
		return err
	}

	if config.Router == nil {
		return nil
	}

	if err = config.Router.AttachClient(base); err != nil {
		return fmt.Errorf("failed to attach client %w", err)
	}

	channel.OnClose(func() {
		_ = base.Close()
	})

	for {
		msg, err := base.ReadMessage()
		if err != nil {
			_ = config.Router.DetachClient(base)
			break
		}

		if err = config.Router.ReceiveMessage(base, msg); err != nil {
			log.Debugf("failed to receive message for session %s: %v", sessionID, err)
			return nil
		}
	}

	return err
}

func (r *WebRTCProvider) offerFunc(_ context.Context, invocation *xconn.Invocation) *xconn.InvocationResult {
	if len(invocation.Args()) < 1 {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, "must be called with offer as argument")
	}

	offerJSON, err := invocation.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, "offer JSON must be a string")
	}

	var offer Offer
	if err := json.Unmarshal([]byte(offerJSON), &offer); err != nil {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, fmt.Sprintf("invalid offer: %v", err))
	}

	r.Lock()
	cfg := &AnswerConfig{ICEServers: cloneICEServers(r.iceServers)}
	r.Unlock()
	requestID := uuid.New().String()

	answer, err := r.handleOffer(requestID, offer, cfg)
	if err != nil {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, err)
	}

	responseData, err := json.Marshal(OfferResponse{
		RequestID: requestID,
		Answer:    *answer,
	})
	if err != nil {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, err)
	}

	return xconn.NewInvocationResult(string(responseData))
}

func (r *WebRTCProvider) onRemoteCandidate(event *xconn.Event) {
	if len(event.Args()) < 2 {
		return
	}

	requestID, err := event.ArgString(0)
	if err != nil {
		log.Debugln("request ID must be a string")
		return
	}

	candidateJSON, err := event.ArgString(1)
	if err != nil {
		log.Debugln("offer must be a string")
		return
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(candidateJSON), &candidate); err != nil {
		return
	}

	if err := r.addIceCandidate(requestID, candidate); err != nil {
		log.Debugf("failed to add ice candidate: %v", err)
		return
	}
}
