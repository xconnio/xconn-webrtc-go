package xconnwebrtc

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

type pendingRemoteCandidate struct {
	requestID string
	candidate webrtc.ICECandidateInit
}

type ClientConfig struct {
	Realm                    string
	ProcedureWebRTCOffer     string
	TopicAnswererOnCandidate string
	TopicOffererOnCandidate  string
	ConnectTimeout           time.Duration
	Serializer               xconn.SerializerSpec
	Authenticator            auth.ClientAuthenticator
	Session                  *xconn.Session
	ICEServers               []webrtc.ICEServer

	OnDisconnect func()
}

func (c *ClientConfig) validate() error {
	if c == nil {
		return fmt.Errorf("client config is nil")
	}
	if c.Realm == "" {
		return fmt.Errorf("realm must not be empty")
	}
	if c.ProcedureWebRTCOffer == "" {
		return fmt.Errorf("ProcedureWebRTCOffer must not be empty")
	}
	if c.TopicAnswererOnCandidate == "" {
		return fmt.Errorf("TopicAnswererOnCandidate must not be empty")
	}
	if c.TopicOffererOnCandidate == "" {
		return fmt.Errorf("TopicOffererOnCandidate must not be empty")
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 20 * time.Second
	}
	if c.Serializer == nil {
		c.Serializer = xconn.JSONSerializerSpec
	}
	if c.Authenticator == nil {
		c.Authenticator = auth.NewAnonymousAuthenticator("", nil)
	}
	if c.Session == nil {
		return fmt.Errorf("session must not be nil")
	}
	return nil
}

// connectWebRTC runs the offer/answer/ICE exchange and returns the resulting
// PeerConnection and its first (signaling) DataChannel, before any WAMP
// handshake or join happens on it.
func connectWebRTC(config *ClientConfig) (*webrtc.PeerConnection, *webrtc.DataChannel, error) {
	if err := config.validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid client config: %w", err)
	}
	offerer := NewOfferer()
	var (
		mu                sync.Mutex
		requestID         string
		pendingCandidates []pendingRemoteCandidate
	)
	offerConfig := &OfferConfig{
		ICEServers:               cloneICEServers(config.ICEServers),
		Ordered:                  true,
		TopicAnswererOnCandidate: config.TopicAnswererOnCandidate,
	}

	subscribeResponse := config.Session.Subscribe(config.TopicOffererOnCandidate, func(event *xconn.Event) {
		if len(event.Args()) < 2 {
			log.Debugf("invalid arguments length")
			return
		}

		candidateRequestID, err := event.ArgString(0)
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
			log.Debugln(err)
			return
		}

		mu.Lock()
		if requestID == "" {
			pendingCandidates = append(pendingCandidates, pendingRemoteCandidate{
				requestID: candidateRequestID,
				candidate: candidate,
			})
			mu.Unlock()
			return
		}
		match := candidateRequestID == requestID
		mu.Unlock()

		if !match {
			log.Debugf("invalid requestID")
			return
		}

		if err = offerer.AddICECandidate(candidate); err != nil {
			log.Debugln(err)
		}
	}).Do()
	if subscribeResponse.Err != nil {
		return nil, nil, subscribeResponse.Err
	}
	defer func() {
		if err := subscribeResponse.Unsubscribe(); err != nil {
			log.Debugf("failed to unsubscribe from offerer candidates: %v", err)
		}
	}()

	offer, err := offerer.Offer(offerConfig)
	if err != nil {
		return nil, nil, err
	}

	offerJSON, err := json.Marshal(offer)
	if err != nil {
		return nil, nil, err
	}

	callResponse := config.Session.Call(config.ProcedureWebRTCOffer).Args(string(offerJSON)).Do()
	if callResponse.Err != nil {
		return nil, nil, callResponse.Err
	}

	offerResponseText, err := callResponse.ArgString(0)
	if err != nil {
		return nil, nil, err
	}
	var offerResponse OfferResponse
	if err = json.Unmarshal([]byte(offerResponseText), &offerResponse); err != nil {
		return nil, nil, err
	}
	if offerResponse.RequestID == "" {
		return nil, nil, fmt.Errorf("offer response request ID must not be empty")
	}

	mu.Lock()
	requestID = offerResponse.RequestID
	buffered := pendingCandidates
	pendingCandidates = nil
	mu.Unlock()

	for _, pc := range buffered {
		if pc.requestID != requestID {
			continue
		}
		if err = offerer.AddICECandidate(pc.candidate); err != nil {
			log.Debugln(err)
		}
	}

	offerer.StartICETrickle(config.Session, offerConfig.TopicAnswererOnCandidate, requestID)

	if err = offerer.HandleAnswer(offerResponse.Answer); err != nil {
		return nil, nil, err
	}

	channel, err := waitForDataChannel(offerer.connection, offerer.WaitReady(), config.ConnectTimeout)
	if err != nil {
		if offerer.connection != nil {
			_ = offerer.connection.Close()
		}
		return nil, nil, err
	}

	return offerer.connection, channel, nil
}

func waitForDataChannel(connection *webrtc.PeerConnection, ready <-chan *webrtc.DataChannel,
	timeout time.Duration) (*webrtc.DataChannel, error) {
	errCh := make(chan error, 1)
	if connection != nil {
		connection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			switch state {
			case webrtc.PeerConnectionStateFailed:
				select {
				case errCh <- fmt.Errorf("webrtc connection failed before data channel opened"):
				default:
				}
			case webrtc.PeerConnectionStateDisconnected:
				select {
				case errCh <- fmt.Errorf("webrtc connection disconnected before data channel opened"):
				default:
				}
			case webrtc.PeerConnectionStateClosed:
				select {
				case errCh <- fmt.Errorf("webrtc connection closed before data channel opened"):
				default:
				}
			default:
			}
		})
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case channel := <-ready:
		if channel == nil {
			return nil, fmt.Errorf("webrtc data channel was not created")
		}
		return channel, nil
	case err := <-errCh:
		return nil, err
	case <-timer.C:
		return nil, fmt.Errorf("webrtc connection timed out after %s waiting for data channel", timeout)
	}
}

// ConnectWAMP establishes a WebRTC PeerConnection and joins realm over its
// first DataChannel, returning a *WebRTCSession. Since WebRTCSession extends
// *xconn.Session, the result is immediately usable for WAMP calls, and also
// exposes the underlying connection for opening more sessions or raw data
// channels (see WebRTCSession.OpenSession / OpenChannel / OnDataChannel).
func ConnectWAMP(config *ClientConfig) (*WebRTCSession, error) {
	connection, channel, err := connectWebRTC(config)
	if err != nil {
		return nil, err
	}

	session, err := joinWebRTCSession(connection, channel, config.Realm,
		config.Serializer, config.Authenticator, config.ConnectTimeout)
	if err != nil {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, err
	}

	// Closing this session's own channel (handled inside joinWebRTCSession)
	// already covers connection failure, since pion cascades the failure to
	// every channel's own read loop. This handler only needs to surface the
	// connection-level OnDisconnect callback.
	connection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			if config.OnDisconnect != nil {
				config.OnDisconnect()
			}
		default:
		}
	})

	return session, nil
}

// joinWebRTCSession performs the magic-byte handshake and WAMP join over an
// already-open channel, wrapping the result in a WebRTCSession that shares
// connection. Used both for a brand new PeerConnection's first channel and
// for additional channels opened via WebRTCSession.OpenSession.
func joinWebRTCSession(connection *webrtc.PeerConnection, channel *webrtc.DataChannel, realm string,
	spec xconn.SerializerSpec, authenticator auth.ClientAuthenticator, timeout time.Duration) (*WebRTCSession, error) {

	if err := sendClientHandshake(channel, spec, timeout); err != nil {
		return nil, err
	}

	peer := NewWebRTCPeer(channel)
	base, err := xconn.Join(peer, realm, spec.Serializer(), authenticator)
	if err != nil {
		return nil, err
	}

	channel.OnClose(func() {
		_ = base.Close()
	})

	return &WebRTCSession{
		Session:    xconn.NewSession(base, spec.Serializer()),
		connection: connection,
		channel:    channel,
	}, nil
}
