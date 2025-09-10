package wamp_webrtc_go

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
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
	Serializer               xconn.SerializerSpec
	Authenticator            auth.ClientAuthenticator
	Session                  *xconn.Session
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
	offerConfig := &OfferConfig{
		Protocol:                 config.Serializer.SubProtocol(),
		ICEServers:               []webrtc.ICEServer{},
		Ordered:                  true,
		TopicAnswererOnCandidate: config.TopicAnswererOnCandidate,
	}

	subscribeResponse := config.Session.Subscribe(config.TopicOffererOnCandidate, func(event *xconn.Event) {
		if len(event.Args()) < 2 {
			log.Errorf("invalid arguments length")
			return
		}

		candidateJSON, err := event.ArgString(1)
		if err != nil {
			log.Errorln("offer must be a string")
			return
		}

		var candidate webrtc.ICECandidateInit
		if err := json.Unmarshal([]byte(candidateJSON), &candidate); err != nil {
			log.Errorln(err)
			return
		}

		if err = offerer.AddICECandidate(candidate); err != nil {
			log.Errorln(err)
		}
	}).Do()
	if subscribeResponse.Err != nil {
		return nil, subscribeResponse.Err
	}

	requestID := uuid.New().String()
	offer, err := offerer.Offer(offerConfig, config.Session, requestID)
	if err != nil {
		return nil, err
	}

	offerJSON, err := json.Marshal(offer)
	if err != nil {
		return nil, err
	}

	callResponse := config.Session.Call(config.ProcedureWebRTCOffer).Args(requestID, string(offerJSON)).Do()
	if callResponse.Err != nil {
		return nil, callResponse.Err
	}

	answerText, err := callResponse.Args[0].String()
	if err != nil {
		return nil, err
	}
	var answer Answer
	if err = json.Unmarshal([]byte(answerText), &answer); err != nil {
		return nil, err
	}

	if err = offerer.HandleAnswer(answer); err != nil {
		return nil, err
	}

	channel := <-offerer.WaitReady()

	return &WebRTCSession{
		Channel:    channel,
		Connection: offerer.connection,
	}, nil
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

	wampSession := xconn.NewSession(base, config.Serializer.Serializer())

	return wampSession, nil
}
