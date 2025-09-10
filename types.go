package wamp_webrtc_go

import (
	"fmt"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
)

type Answer struct {
	Candidates  []webrtc.ICECandidateInit `json:"candidates"`
	Description webrtc.SessionDescription `json:"description"`
}

type Offer = Answer

type OfferConfig struct {
	Protocol                 string
	ICEServers               []webrtc.ICEServer
	Ordered                  bool
	ID                       uint16
	TopicAnswererOnCandidate string
}

type AnswerConfig struct {
	ICEServers []webrtc.ICEServer
}

type ProviderConfig struct {
	Session                     *xconn.Session
	ProcedureHandleOffer        string
	TopicHandleRemoteCandidates string
	TopicPublishLocalCandidate  string
	Serializer                  serializers.Serializer
	Routed                      bool
	Authenticator               auth.ServerAuthenticator
	IceServers                  []webrtc.ICEServer
}

func (c *ProviderConfig) validate() error {
	if c == nil {
		return fmt.Errorf("provider config is nil")
	}
	if c.Session == nil {
		return fmt.Errorf("session must not be nil")
	}
	if c.ProcedureHandleOffer == "" {
		return fmt.Errorf("procedureHandleOffer must not be empty")
	}
	if c.TopicHandleRemoteCandidates == "" {
		return fmt.Errorf("topicHandleRemoteCandidates must not be empty")
	}
	if c.TopicPublishLocalCandidate == "" {
		return fmt.Errorf("topicPublishLocalCandidate must not be empty")
	}
	if c.Serializer == nil {
		c.Serializer = &serializers.JSONSerializer{}
	}
	return nil
}

type WebRTCSession struct {
	Connection *webrtc.PeerConnection
	Channel    *webrtc.DataChannel
}

func (w *WebRTCSession) OpenChannel(label string, options *webrtc.DataChannelInit) (*webrtc.DataChannel, error) {
	return w.Connection.CreateDataChannel(label, options)
}
