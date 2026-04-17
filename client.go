package xconnwebrtc

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

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

func connectWebRTC(config *ClientConfig) (*WebRTCSession, error) {
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid client config: %w", err)
	}
	offerer := NewOfferer()
	var requestID string
	offerConfig := &OfferConfig{
		Protocol:                 config.Serializer.SubProtocol(),
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
		if candidateRequestID != requestID {
			log.Debugf("invalid requestID")
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

		if err = offerer.AddICECandidate(candidate); err != nil {
			log.Debugln(err)
		}
	}).Do()
	if subscribeResponse.Err != nil {
		return nil, subscribeResponse.Err
	}
	defer func() {
		if err := subscribeResponse.Unsubscribe(); err != nil {
			log.Debugf("failed to unsubscribe from offerer candidates: %v", err)
		}
	}()

	offer, err := offerer.Offer(offerConfig)
	if err != nil {
		return nil, err
	}

	offerJSON, err := json.Marshal(offer)
	if err != nil {
		return nil, err
	}

	callResponse := config.Session.Call(config.ProcedureWebRTCOffer).Args(string(offerJSON)).Do()
	if callResponse.Err != nil {
		return nil, callResponse.Err
	}

	offerResponseText, err := callResponse.ArgString(0)
	if err != nil {
		return nil, err
	}
	var offerResponse OfferResponse
	if err = json.Unmarshal([]byte(offerResponseText), &offerResponse); err != nil {
		return nil, err
	}
	requestID = offerResponse.RequestID
	if requestID == "" {
		return nil, fmt.Errorf("offer response request ID must not be empty")
	}

	offerer.StartICETrickle(config.Session, offerConfig.TopicAnswererOnCandidate, requestID)

	if err = offerer.HandleAnswer(offerResponse.Answer); err != nil {
		return nil, err
	}

	channel, err := waitForDataChannel(offerer.connection, offerer.WaitReady(), config.ConnectTimeout)
	if err != nil {
		if offerer.connection != nil {
			_ = offerer.connection.Close()
		}
		return nil, err
	}

	return &WebRTCSession{
		Channel:    channel,
		Connection: offerer.connection,
	}, nil
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

func ConnectWebRTC(config *ClientConfig) (*WebRTCSession, error) {
	webRTCSession, err := connectWebRTC(config)
	if err != nil {
		return nil, err
	}

	peer := NewWebRTCPeer(webRTCSession.Channel)
	_, err = xconn.Join(peer, config.Realm, config.Serializer.Serializer(), config.Authenticator)
	if err != nil {
		return nil, err
	}

	return &WebRTCSession{
		Channel:    webRTCSession.Channel,
		Connection: webRTCSession.Connection,
	}, nil
}

func ConnectWAMP(config *ClientConfig) (*xconn.Session, error) {
	webRTCConnection, err := connectWebRTC(config)
	if err != nil {
		return nil, err
	}

	peer := NewWebRTCPeer(webRTCConnection.Channel)
	base, err := xconn.Join(peer, config.Realm, config.Serializer.Serializer(), config.Authenticator)
	if err != nil {
		return nil, err
	}

	webRTCConnection.Channel.OnClose(func() {
		_ = base.Close()
	})

	webRTCConnection.Connection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			_ = base.Close()
		default:
		}
	})

	wampSession := xconn.NewSession(base, config.Serializer.Serializer())

	return wampSession, nil
}

func ConnectWAMPAndWebRTC(config *ClientConfig) (*xconn.Session, *webrtc.DataChannel, error) {
	webRTCConnection, err := connectWebRTC(config)
	if err != nil {
		return nil, nil, err
	}

	peer := NewWebRTCPeer(webRTCConnection.Channel)

	base, err := xconn.Join(peer, config.Realm, config.Serializer.Serializer(), config.Authenticator)
	if err != nil {
		return nil, nil, err
	}

	wampSession := xconn.NewSession(base, config.Serializer.Serializer())

	return wampSession, webRTCConnection.Channel, nil
}
