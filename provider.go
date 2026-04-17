package xconnwebrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

type WebRTCProvider struct {
	answerers     map[string]*Answerer
	onNewAnswerer func(sessionID string, answerer *Answerer)

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
			case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed,
				webrtc.PeerConnectionStateClosed:
				r.removeAnswerer(requestID, answerer)
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

		go func() {
			select {
			case channel := <-answerer.WaitReady():
				if err := r.handleWAMPClient(sessionID, answerer, channel, config); err != nil {
					log.Debugf("failed to handle answer: %v", err)
					r.removeAnswerer(sessionID, answerer)
				}
			case <-time.After(20 * time.Second):
				log.Debugln("webrtc connection didn't establish after 20 seconds")
				r.removeAnswerer(sessionID, answerer)
			}
		}()
	})

	return nil
}

func (r *WebRTCProvider) handleWAMPClient(sessionID string, answerer *Answerer,
	channel *webrtc.DataChannel, config *ProviderConfig) error {
	defer r.removeAnswerer(sessionID, answerer)

	rtcPeer := NewWebRTCPeer(channel)

	hello, err := xconn.ReadHello(rtcPeer, config.Serializer)
	if err != nil {
		return err
	}

	base, err := xconn.Accept(rtcPeer, hello, config.Serializer, config.Authenticator)
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
		r.removeAnswerer(sessionID, answerer)
	})

	for {
		msg, err := base.ReadMessage()
		if err != nil {
			_ = config.Router.DetachClient(base)
			break
		}

		if err = config.Router.ReceiveMessage(base, msg); err != nil {
			log.Println(err)
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
