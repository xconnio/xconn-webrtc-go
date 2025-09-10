package wamp_webrtc_go

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

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
		answerers:  make(map[string]*Answerer),
		iceServers: make([]webrtc.ICEServer, 0),
	}
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

func (r *WebRTCProvider) addIceCandidate(requestID string, candidate webrtc.ICECandidateInit) error {
	answerer := r.ensureAnswerer(requestID)
	return answerer.AddICECandidate(candidate)
}

func (r *WebRTCProvider) handleOffer(requestID string, offer Offer, answerConfig *AnswerConfig) (*Answer, error) {
	answerer := r.ensureAnswerer(requestID)
	return answerer.Answer(answerConfig, offer, 100*time.Millisecond)
}

func (r *WebRTCProvider) Setup(config *ProviderConfig) error {
	if err := config.validate(); err != nil {
		return fmt.Errorf("invalid provider config: %w", err)
	}
	r.iceServers = append(r.iceServers, config.IceServers...)
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
				log.Errorf("failed to marshal answer: %v", err)
				return
			}

			args := []any{sessionID, string(answerData)}
			publishResp := config.Session.Publish(config.TopicPublishLocalCandidate).Args(args...).Do()
			if publishResp.Err != nil {
				log.Errorf("failed to publish answer: %v", publishResp.Err)
			}
		})

		go func() {
			select {
			case channel := <-answerer.WaitReady():
				if err := r.handleWAMPClient(channel, config); err != nil {
					log.Errorf("failed to handle answer: %v", err)
					_ = answerer.connection.Close()
				}
			case <-time.After(20 * time.Second):
				log.Errorln("webrtc connection didn't establish after 20 seconds")
			}
		}()
	})

	return nil
}

func (r *WebRTCProvider) handleWAMPClient(channel *webrtc.DataChannel, config *ProviderConfig) error {
	rtcPeer := NewWebRTCPeer(channel)

	hello, err := xconn.ReadHello(rtcPeer, config.Serializer)
	if err != nil {
		return err
	}

	base, err := xconn.Accept(rtcPeer, hello, config.Serializer, config.Authenticator)
	if err != nil {
		return err
	}

	if !config.Routed {
		return nil
	}

	xconnRouter := xconn.NewRouter()
	if err := xconnRouter.AddRealm("realm1"); err != nil {
		return err
	}
	if err = xconnRouter.AttachClient(base); err != nil {
		return fmt.Errorf("failed to attach client %w", err)
	}

	parser := NewWebRTCMessageAssembler()
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		fullMsg := parser.Feed(msg.Data)

		if fullMsg != nil {
			if err = base.Write(fullMsg); err != nil {
				log.Errorf("failed to send wamp message: %v", err)
				return
			}
		}
	})

	channel.OnClose(func() {
		_ = base.Close()
	})

	for {
		msg, err := base.ReadMessage()
		if err != nil {
			_ = xconnRouter.DetachClient(base)
			break
		}

		if err = xconnRouter.ReceiveMessage(base, msg); err != nil {
			log.Println(err)
			return nil
		}

		data, err := config.Serializer.Serialize(msg)
		if err != nil {
			log.Printf("failed to serialize message: %v", err)
			return nil
		}

		for chunk := range parser.ChunkMessage(data) {
			if err = channel.Send(chunk); err != nil {
				log.Errorf("failed to write message: %v", err)
				return nil
			}
		}
	}

	return err
}

func (r *WebRTCProvider) offerFunc(_ context.Context, invocation *xconn.Invocation) *xconn.InvocationResult {
	if len(invocation.Args()) < 2 {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument)
	}

	requestID, err := invocation.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, "request ID must be a string")
	}

	offerJSON, err := invocation.ArgString(1)
	if err != nil {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, "offer JSON must be a string")
	}

	var offer Offer
	if err := json.Unmarshal([]byte(offerJSON), &offer); err != nil {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, err.Error())
	}

	r.iceServers = append(r.iceServers, webrtc.ICEServer{URLs: []string{"stun:stun.l.google.com:19302"}})

	cfg := &AnswerConfig{ICEServers: r.iceServers}

	answer, err := r.handleOffer(requestID, offer, cfg)
	if err != nil {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, err.Error())
	}

	answerData, err := json.Marshal(answer)
	if err != nil {
		return xconn.NewInvocationError(wampproto.ErrInvalidArgument, err.Error())
	}

	return xconn.NewInvocationResult(string(answerData))
}

func (r *WebRTCProvider) onRemoteCandidate(event *xconn.Event) {
	if len(event.Args()) < 2 {
		return
	}

	requestID, err := event.ArgString(0)
	if err != nil {
		log.Errorln("request ID must be a string")
		return
	}

	candidateJSON, err := event.ArgString(1)
	if err != nil {
		log.Errorln("offer must be a string")
		return
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(candidateJSON), &candidate); err != nil {
		return
	}

	if err := r.addIceCandidate(requestID, candidate); err != nil {
		log.Errorf("failed to add ice candidate: %v", err)
		return
	}
}
